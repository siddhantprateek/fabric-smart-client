/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package generic

import (
	"strconv"
	"sync"
	"time"

	"github.com/hyperledger-labs/fabric-smart-client/platform/fabric/core/generic/committer"
	"github.com/hyperledger-labs/fabric-smart-client/platform/fabric/core/generic/rwset"
	"github.com/hyperledger-labs/fabric-smart-client/platform/fabric/driver"
	"github.com/hyperledger-labs/fabric-smart-client/platform/view/services/grpc"
	"github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/bccsp/factory"
	"github.com/hyperledger/fabric/common/channelconfig"
	"github.com/hyperledger/fabric/common/configtx"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
)

const (
	channelConfigKey = "CHANNEL_CONFIG_ENV_BYTES"
	peerNamespace    = "_configtx"
)

// TODO: introduced due to a race condition in idemix.
var commitConfigMutex = &sync.Mutex{}

func (c *channel) ReloadConfigTransactions() error {
	c.applyLock.Lock()
	defer c.applyLock.Unlock()

	qe, err := c.vault.NewQueryExecutor()
	if err != nil {
		return errors.WithMessagef(err, "failed getting query executor")
	}
	defer qe.Done()

	logger.Infof("looking up the latest config block available")
	var sequence uint64 = 1
	for {
		txID := committer.ConfigTXPrefix + strconv.FormatUint(sequence, 10)
		vc, err := c.vault.Status(txID)
		if err != nil {
			return errors.WithMessagef(err, "failed getting tx's status [%s]", txID)
		}
		done := false
		switch vc {
		case driver.Valid:
			logger.Infof("config block available, txID [%s], loading...", txID)

			key, err := rwset.CreateCompositeKey(channelConfigKey, []string{strconv.FormatUint(sequence, 10)})
			if err != nil {
				return errors.Wrapf(err, "cannot create configtx rws key")
			}
			envelope, err := qe.GetState(peerNamespace, key)
			if err != nil {
				return errors.Wrapf(err, "failed setting configtx state in rws")
			}
			env, err := protoutil.UnmarshalEnvelope(envelope)
			if err != nil {
				return errors.Wrapf(err, "cannot get payload from config transaction [%s]", txID)
			}
			payload, err := protoutil.UnmarshalPayload(env.Payload)
			if err != nil {
				return errors.Wrapf(err, "cannot get payload from config transaction [%s]", txID)
			}
			ctx, err := configtx.UnmarshalConfigEnvelope(payload.Data)
			if err != nil {
				return errors.Wrapf(err, "error unmarshalling config which passed initial validity checks [%s]", txID)
			}

			var bundle *channelconfig.Bundle
			if c.Resources() == nil {
				// setup the genesis block
				bundle, err = channelconfig.NewBundle(c.name, ctx.Config, factory.GetDefault())
				if err != nil {
					return errors.Wrapf(err, "failed to build a new bundle")
				}
			} else {
				configTxValidator := c.Resources().ConfigtxValidator()
				err := configTxValidator.Validate(ctx)
				if err != nil {
					return errors.Wrapf(err, "failed to validate config transaction [%s]", txID)
				}

				bundle, err = channelconfig.NewBundle(configTxValidator.ChannelID(), ctx.Config, factory.GetDefault())
				if err != nil {
					return errors.Wrapf(err, "failed to create next bundle")
				}

				channelconfig.LogSanityChecks(bundle)
				if err := capabilitiesSupported(bundle); err != nil {
					return err
				}
			}

			c.applyBundle(bundle)

			sequence = sequence + 1
			continue
		case driver.Unknown:
			done = true
		default:
			return errors.Errorf("invalid configtx's [%s] status [%d]", txID, vc)
		}
		if done {
			break
		}
	}
	if sequence == 1 {
		logger.Infof("no config block available, must start from genesis")
		// no configuration block found
		return nil
	}
	logger.Infof("latest config block available at sequence [%d]", sequence-1)

	return nil
}

// CommitConfig is used to validate and apply configuration transactions for a channel.
func (c *channel) CommitConfig(blockNumber uint64, raw []byte, env *common.Envelope) error {
	commitConfigMutex.Lock()
	defer commitConfigMutex.Unlock()

	c.applyLock.Lock()
	defer c.applyLock.Unlock()

	logger.Debugf("[channel: %s] received config transaction number %d", c.name, blockNumber)

	if env == nil {
		return errors.Errorf("channel config found nil")
	}

	payload, err := protoutil.UnmarshalPayload(env.Payload)
	if err != nil {
		return errors.Wrapf(err, "cannot get payload from config transaction, block number [%d]", blockNumber)
	}

	ctx, err := configtx.UnmarshalConfigEnvelope(payload.Data)
	if err != nil {
		return errors.Wrapf(err, "error unmarshalling config which passed initial validity checks")
	}

	txid := committer.ConfigTXPrefix + strconv.FormatUint(ctx.Config.Sequence, 10)
	vc, err := c.vault.Status(txid)
	if err != nil {
		return errors.Wrapf(err, "failed getting tx's status [%s]", txid)
	}
	switch vc {
	case driver.Valid:
		return nil
	case driver.Unknown:
		// this is okay
	default:
		return errors.Errorf("invalid configtx's [%s] status [%d]", txid, vc)
	}

	var bundle *channelconfig.Bundle
	if c.Resources() == nil {
		// setup the genesis block
		bundle, err = channelconfig.NewBundle(c.name, ctx.Config, factory.GetDefault())
		if err != nil {
			return errors.Wrapf(err, "failed to build a new bundle")
		}
	} else {
		configTxValidator := c.Resources().ConfigtxValidator()
		err := configTxValidator.Validate(ctx)
		if err != nil {
			return errors.Wrapf(err, "failed to validate config transaction, block number [%d]", blockNumber)
		}

		bundle, err = channelconfig.NewBundle(configTxValidator.ChannelID(), ctx.Config, factory.GetDefault())
		if err != nil {
			return errors.Wrapf(err, "failed to create next bundle")
		}

		channelconfig.LogSanityChecks(bundle)
		if err := capabilitiesSupported(bundle); err != nil {
			return err
		}
	}

	if err := c.commitConfig(txid, blockNumber, ctx.Config.Sequence, raw); err != nil {
		return errors.Wrapf(err, "failed committing configtx to the vault")
	}

	c.applyBundle(bundle)

	return nil
}

// Resources returns the active channel configuration bundle.
func (c *channel) Resources() channelconfig.Resources {
	c.lock.RLock()
	res := c.resources
	c.lock.RUnlock()
	return res
}

func (c *channel) commitConfig(txid string, blockNumber uint64, seq uint64, envelope []byte) error {
	rws, err := c.vault.NewRWSet(txid)
	if err != nil {
		return errors.Wrapf(err, "cannot create rws for configtx")
	}
	defer rws.Done()

	key, err := rwset.CreateCompositeKey(channelConfigKey, []string{strconv.FormatUint(seq, 10)})
	if err != nil {
		return errors.Wrapf(err, "cannot create configtx rws key")
	}
	if err := rws.SetState(peerNamespace, key, envelope); err != nil {
		return errors.Wrapf(err, "failed setting configtx state in rws")
	}
	rws.Done()
	if err := c.CommitTX(txid, blockNumber, 0, nil); err != nil {
		if err2 := c.DiscardTx(txid); err2 != nil {
			logger.Errorf("failed committing configtx rws [%s]", err2)
		}
		return errors.Wrapf(err, "failed committing configtx rws")
	}
	return nil
}

func (c *channel) applyBundle(bundle *channelconfig.Bundle) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.resources = bundle

	// update the list of orderers
	orderers, any := c.resources.OrdererConfig()
	if any {
		logger.Debugf("[channel: %s] Orderer config has changed, updating the list of orderers", c.name)

		var newOrderers []*grpc.ConnectionConfig
		orgs := orderers.Organizations()
		for _, org := range orgs {
			msp := org.MSP()
			var tlsRootCerts [][]byte
			tlsRootCerts = append(tlsRootCerts, msp.GetTLSRootCerts()...)
			tlsRootCerts = append(tlsRootCerts, msp.GetTLSIntermediateCerts()...)
			for _, endpoint := range org.Endpoints() {
				logger.Debugf("[channel: %s] Adding orderer endpoint: [%s:%s:%s]", c.name, org.Name(), org.MSPID(), endpoint)
				newOrderers = append(newOrderers, &grpc.ConnectionConfig{
					Address:           endpoint,
					ConnectionTimeout: 10 * time.Second,
					TLSEnabled:        true,
					TLSRootCertBytes:  tlsRootCerts,
				})
			}
		}
		if len(newOrderers) != 0 {
			logger.Debugf("[channel: %s] Updating the list of orderers: (%d) found", c.name, len(newOrderers))
			c.network.setConfigOrderers(newOrderers)
		} else {
			logger.Debugf("[channel: %s] No orderers found in channel config", c.name)
		}
	} else {
		logger.Debugf("no orderer configuration found in channel config")
	}
}

func capabilitiesSupported(res channelconfig.Resources) error {
	ac, ok := res.ApplicationConfig()
	if !ok {
		return errors.Errorf("[channel %s] does not have application config so is incompatible", res.ConfigtxValidator().ChannelID())
	}

	if err := ac.Capabilities().Supported(); err != nil {
		return errors.Wrapf(err, "[channel %s] incompatible", res.ConfigtxValidator().ChannelID())
	}

	if err := res.ChannelConfig().Capabilities().Supported(); err != nil {
		return errors.Wrapf(err, "[channel %s] incompatible", res.ConfigtxValidator().ChannelID())
	}

	return nil
}
