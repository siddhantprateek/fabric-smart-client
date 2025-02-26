/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package idemix

import (
	"testing"

	"github.com/stretchr/testify/assert"

	api2 "github.com/hyperledger-labs/fabric-smart-client/platform/fabric/driver"

	"github.com/hyperledger-labs/fabric-smart-client/platform/view/view"
)

func TestIdentityCache(t *testing.T) {
	c := NewIdentityCache(
		func(opts *api2.IdentityOptions) (view.Identity, []byte, error) {
			return []byte("hello world"), []byte("audit"), nil
		},
		100,
	)
	id, audit, err := c.Identity(&api2.IdentityOptions{
		EIDExtension: true,
		AuditInfo:    nil,
	})
	assert.NoError(t, err)
	assert.Equal(t, view.Identity([]byte("hello world")), id)
	assert.Equal(t, []byte("audit"), audit)

	id, audit, err = c.Identity(nil)
	assert.NoError(t, err)
	assert.Equal(t, view.Identity([]byte("hello world")), id)
	assert.Equal(t, []byte("audit"), audit)
}
