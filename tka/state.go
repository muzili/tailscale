// Copyright (c) 2022 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package tka

import (
	"bytes"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// ErrNoSuchKey is returned if the key referenced by a KeyID does not exist.
var ErrNoSuchKey = errors.New("key not found")

// State describes Tailnet Key Authority state at an instant in time.
//
// State is mutated by applying Authority Update Messages (AUMs), resulting
// in a new State.
type State struct {
	// LastAUMHash is the blake2s digest of the last-applied AUM.
	// Because AUMs are strictly ordered and form a hash chain, we
	// check the previous AUM hash in an update we are applying
	// is the same as the LastAUMHash.
	LastAUMHash *AUMHash `cbor:"1,keyasint"`

	// DisablementSecrets are KDF-derived values which can be used
	// to turn off the TKA in the event of a consensus-breaking bug.
	// An AUM of type DisableNL should contain a secret when results
	// in one of these values when run through the disablement KDF.
	//
	// TODO(tom): This is an alpha feature, remove this mechanism once
	//            we have confidence in our implementation.
	DisablementSecrets [][]byte `cbor:"2,keyasint"`

	// Keys are the public keys currently trusted by the TKA.
	Keys []Key `cbor:"3,keyasint"`
}

// GetKey returns the trusted key with the specified KeyID.
func (s State) GetKey(key KeyID) (Key, error) {
	for _, k := range s.Keys {
		if bytes.Equal(k.ID(), key) {
			return k, nil
		}
	}

	return Key{}, ErrNoSuchKey
}

// Clone makes an independent copy of State.
//
// NOTE: There is a difference between a nil slice and an empty
// slice for encoding purposes, so an implementation of Clone()
// must take care to preserve this.
func (s State) Clone() State {
	out := State{}

	if s.LastAUMHash != nil {
		dupe := *s.LastAUMHash
		out.LastAUMHash = &dupe
	}

	if s.DisablementSecrets != nil {
		out.DisablementSecrets = make([][]byte, len(s.DisablementSecrets))
		for i := range s.DisablementSecrets {
			out.DisablementSecrets[i] = make([]byte, len(s.DisablementSecrets[i]))
			copy(out.DisablementSecrets[i], s.DisablementSecrets[i])
		}
	}

	if s.Keys != nil {
		out.Keys = make([]Key, len(s.Keys))
		for i := range s.Keys {
			out.Keys[i] = s.Keys[i].Clone()
		}
	}

	return out
}

// cloneForUpdate is like Clone, except LastAUMHash is set based
// on the hash of the given update.
func (s State) cloneForUpdate(update *AUM) State {
	out := s.Clone()
	aumHash := update.Hash()
	out.LastAUMHash = &aumHash
	return out
}

const disablementLength = 32

var disablementSalt = []byte("tailscale network-lock disablement salt")

func disablementKDF(secret []byte) []byte {
	// time = 4 (3 recommended, booped to 4 to compensate for less memory)
	// memory = 16 (32 recommended)
	// threads = 4
	// keyLen = 32 (256 bits)
	return argon2.Key(secret, disablementSalt, 4, 16*1024, 4, disablementLength)
}

// checkDisablement returns true for a valid disablement secret.
func (s State) checkDisablement(secret []byte) bool {
	derived := disablementKDF(secret)
	for _, candidate := range s.DisablementSecrets {
		if bytes.Equal(derived, candidate) {
			return true
		}
	}
	return false
}

// parentMatches returns true if an AUM can chain to (be applied)
// to the current state.
//
// Specifically, the rules are:
//   - The last AUM hash must match (transitively, this implies that this
//     update follows the last update message applied to the state machine)
//   - Or, the state machine knows no parent (its brand new).
func (s State) parentMatches(update AUM) bool {
	if s.LastAUMHash == nil {
		return true
	}
	return bytes.Equal(s.LastAUMHash[:], update.PrevAUMHash)
}

// applyVerifiedAUM computes a new state based on the update provided.
//
// The provided update MUST be verified: That is, the AUM must be well-formed
// (as defined by StaticValidate()), and signatures over the AUM must have
// been verified.
func (s State) applyVerifiedAUM(update AUM) (State, error) {
	// Validate that the update message has the right parent.
	if !s.parentMatches(update) {
		return State{}, errors.New("parent AUMHash mismatch")
	}

	switch update.MessageKind {
	case AUMNoOp:
		out := s.cloneForUpdate(&update)
		return out, nil

	case AUMCheckpoint:
		return update.State.cloneForUpdate(&update), nil

	case AUMAddKey:
		if _, err := s.GetKey(update.Key.ID()); err == nil {
			return State{}, errors.New("key already exists")
		}
		out := s.cloneForUpdate(&update)
		out.Keys = append(out.Keys, *update.Key)
		return out, nil

	case AUMUpdateKey:
		k, err := s.GetKey(update.KeyID)
		if err != nil {
			return State{}, err
		}
		if update.Votes != nil {
			k.Votes = *update.Votes
		}
		if update.Meta != nil {
			k.Meta = update.Meta
		}
		out := s.cloneForUpdate(&update)
		for i := range out.Keys {
			if bytes.Equal(out.Keys[i].ID(), update.KeyID) {
				out.Keys[i] = k
			}
		}
		return out, nil

	case AUMRemoveKey:
		idx := -1
		for i := range s.Keys {
			if bytes.Equal(update.KeyID, s.Keys[i].ID()) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return State{}, ErrNoSuchKey
		}
		out := s.cloneForUpdate(&update)
		out.Keys = append(out.Keys[:idx], out.Keys[idx+1:]...)
		return out, nil

	case AUMDisableNL:
		// TODO(tom): We should handle this at a higher level than State.
		if !s.checkDisablement(update.DisablementSecret) {
			return State{}, errors.New("incorrect disablement secret")
		}
		// Valid disablement secret, lets reset
		return State{}, nil

	default:
		// TODO(tom): Instead of erroring, update lastHash and
		// continue (to preserve future compatibility).
		return State{}, fmt.Errorf("unhandled message: %v", update.MessageKind)
	}
}
