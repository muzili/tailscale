// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package key

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"errors"

	"go4.org/mem"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"
	"tailscale.com/types/structs"
)

const (
	// nodePrivateHexPrefix is the prefix used to identify a
	// hex-encoded node private key.
	//
	// This prefix name is a little unfortunate, in that it comes from
	// WireGuard's own key types, and we've used it for both key types
	// we persist to disk (machine and node keys). But we're stuck
	// with it for now, barring another round of tricky migration.
	nodePrivateHexPrefix = "privkey:"

	// nodePublicHexPrefix is the prefix used to identify a
	// hex-encoded node public key.
	//
	// This prefix is used in the control protocol, so cannot be
	// changed.
	nodePublicHexPrefix = "nodekey:"

	// NodePublicRawLen is the length in bytes of a NodePublic, when
	// serialized with AppendTo, Raw32 or WriteRawWithoutAllocating.
	NodePublicRawLen = 32
)

// NodePrivate is a node key, used for WireGuard tunnels and
// communication with DERP servers.
type NodePrivate struct {
	_ structs.Incomparable // because == isn't constant-time
	k [32]byte
}

// NewNode creates and returns a new node private key.
func NewNode() NodePrivate {
	var ret NodePrivate
	rand(ret.k[:])
	// WireGuard does its own clamping, so this would be unnecessary -
	// but we also use this key for DERP comms, which does require
	// clamping.
	clamp25519Private(ret.k[:])
	return ret
}

// NodePrivateFromRaw32 parses a 32-byte raw value as a NodePrivate.
//
// Deprecated: only needed to cast from legacy node private key types,
// do not add more uses unrelated to #3206.
func NodePrivateFromRaw32(raw mem.RO) NodePrivate {
	if raw.Len() != 32 {
		panic("input has wrong size")
	}
	var ret NodePrivate
	raw.Copy(ret.k[:])
	return ret
}

func ParseNodePrivateUntyped(raw mem.RO) (NodePrivate, error) {
	var ret NodePrivate
	if err := parseHex(ret.k[:], raw, mem.B(nil)); err != nil {
		return NodePrivate{}, err
	}
	return ret, nil
}

// IsZero reports whether k is the zero value.
func (k NodePrivate) IsZero() bool {
	return k.Equal(NodePrivate{})
}

// Equal reports whether k and other are the same key.
func (k NodePrivate) Equal(other NodePrivate) bool {
	return subtle.ConstantTimeCompare(k.k[:], other.k[:]) == 1
}

// Public returns the NodePublic for k.
// Panics if NodePrivate is zero.
func (k NodePrivate) Public() NodePublic {
	if k.IsZero() {
		panic("can't take the public key of a zero NodePrivate")
	}
	var ret NodePublic
	curve25519.ScalarBaseMult(&ret.k, &k.k)
	return ret
}

// MarshalText implements encoding.TextMarshaler.
func (k NodePrivate) MarshalText() ([]byte, error) {
	return toHex(k.k[:], nodePrivateHexPrefix), nil
}

// MarshalText implements encoding.TextUnmarshaler.
func (k *NodePrivate) UnmarshalText(b []byte) error {
	return parseHex(k.k[:], mem.B(b), mem.S(nodePrivateHexPrefix))
}

// SealTo wraps cleartext into a NaCl box (see
// golang.org/x/crypto/nacl) to p, authenticated from k, using a
// random nonce.
//
// The returned ciphertext is a 24-byte nonce concatenated with the
// box value.
func (k NodePrivate) SealTo(p NodePublic, cleartext []byte) (ciphertext []byte) {
	if k.IsZero() || p.IsZero() {
		panic("can't seal with zero keys")
	}
	var nonce [24]byte
	rand(nonce[:])
	return box.Seal(nonce[:], cleartext, &nonce, &p.k, &k.k)
}

// OpenFrom opens the NaCl box ciphertext, which must be a value
// created by SealTo, and returns the inner cleartext if ciphertext is
// a valid box from p to k.
func (k NodePrivate) OpenFrom(p NodePublic, ciphertext []byte) (cleartext []byte, ok bool) {
	if k.IsZero() || p.IsZero() {
		panic("can't open with zero keys")
	}
	if len(ciphertext) < 24 {
		return nil, false
	}
	nonce := (*[24]byte)(ciphertext)
	return box.Open(nil, ciphertext[len(nonce):], nonce, &p.k, &k.k)
}

func (k NodePrivate) UntypedHexString() string {
	return hex.EncodeToString(k.k[:])
}

// NodePublic is the public portion of a NodePrivate.
type NodePublic struct {
	k [32]byte
}

// Shard returns a uint8 number from a public key with
// mostly-uniform distribution, suitable for sharding.
func (p NodePublic) Shard() uint8 {
	// A 25519 public key isn't uniformly random, as it ultimately
	// corresponds to a point on the curve.
	// But we don't need perfectly uniformly-random, we need
	// good-enough-for-sharding random, so we haphazardly
	// combine raw values of the key to give us something sufficient.
	s := uint8(p.k[31]) + uint8(p.k[30]) + uint8(p.k[20])
	return s ^ uint8(p.k[2]+p.k[12])
}

// ParseNodePublicUntyped parses an untyped 64-character hex value
// as a NodePublic.
//
// Deprecated: this function is risky to use, because it cannot verify
// that the hex string was intended to be a NodePublic. This can
// lead to accidentally decoding one type of key as another. For new
// uses that don't require backwards compatibility with the untyped
// string format, please use MarshalText/UnmarshalText.
func ParseNodePublicUntyped(raw mem.RO) (NodePublic, error) {
	var ret NodePublic
	if err := parseHex(ret.k[:], raw, mem.B(nil)); err != nil {
		return NodePublic{}, err
	}
	return ret, nil
}

// NodePublicFromRaw32 parses a 32-byte raw value as a NodePublic.
//
// This should be used only when deserializing a NodePublic from a
// binary protocol.
func NodePublicFromRaw32(raw mem.RO) NodePublic {
	if raw.Len() != 32 {
		panic("input has wrong size")
	}
	var ret NodePublic
	raw.Copy(ret.k[:])
	return ret
}

// IsZero reports whether k is the zero value.
func (k NodePublic) IsZero() bool {
	return k == NodePublic{}
}

// ShortString returns the Tailscale conventional debug representation
// of a public key: the first five base64 digits of the key, in square
// brackets.
func (k NodePublic) ShortString() string {
	return debug32(k.k)
}

// AppendTo appends k, serialized as a 32-byte binary value, to
// buf. Returns the new slice.
func (k NodePublic) AppendTo(buf []byte) []byte {
	return append(buf, k.k[:]...)
}

// ReadRawWithoutAllocating initializes k with bytes read from br.
// The reading is done ~4x slower than io.ReadFull, but in exchange is
// allocation-free.
func (k *NodePublic) ReadRawWithoutAllocating(br *bufio.Reader) error {
	var z NodePublic
	if *k != z {
		return errors.New("refusing to read into non-zero NodePublic")
	}
	// This is ~4x slower than io.ReadFull, but using io.ReadFull
	// causes one extra alloc, which is significant for the DERP
	// server that consumes this method. So, process stuff slower but
	// without allocation.
	//
	// Dear future: if io.ReadFull stops causing stuff to escape, you
	// should switch back to that.
	for i := range k.k {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		k.k[i] = b
	}
	return nil
}

// WriteRawWithoutAllocating writes out k as 32 bytes to bw.
// The writing is done ~3x slower than bw.Write, but in exchange is
// allocation-free.
func (k NodePublic) WriteRawWithoutAllocating(bw *bufio.Writer) error {
	// Equivalent to bw.Write(k.k[:]), but without causing an
	// escape-related alloc.
	//
	// Dear future: if bw.Write(k.k[:]) stops causing stuff to escape,
	// you should switch back to that.
	for _, b := range k.k {
		err := bw.WriteByte(b)
		if err != nil {
			return err
		}
	}
	return nil
}

// Raw32 returns k encoded as 32 raw bytes.
//
// Deprecated: only needed for a single legacy use in the control
// server, don't add more uses.
func (k NodePublic) Raw32() [32]byte {
	var ret [32]byte
	copy(ret[:], k.k[:])
	return ret
}

// Less reports whether k orders before other, using an undocumented
// deterministic ordering.
func (k NodePublic) Less(other NodePublic) bool {
	return bytes.Compare(k.k[:], other.k[:]) < 0
}

// UntypedHexString returns k, encoded as an untyped 64-character hex
// string.
//
// Deprecated: this function is risky to use, because it produces
// serialized values that do not identify themselves as a
// NodePublic, allowing other code to potentially parse it back in
// as the wrong key type. For new uses that don't require backwards
// compatibility with the untyped string format, please use
// MarshalText/UnmarshalText.
func (k NodePublic) UntypedHexString() string {
	return hex.EncodeToString(k.k[:])
}

// String returns the output of MarshalText as a string.
func (k NodePublic) String() string {
	bs, err := k.MarshalText()
	if err != nil {
		panic(err)
	}
	return string(bs)
}

// MarshalText implements encoding.TextMarshaler.
func (k NodePublic) MarshalText() ([]byte, error) {
	return toHex(k.k[:], nodePublicHexPrefix), nil
}

// MarshalText implements encoding.TextUnmarshaler.
func (k *NodePublic) UnmarshalText(b []byte) error {
	return parseHex(k.k[:], mem.B(b), mem.S(nodePublicHexPrefix))
}

// WireGuardGoString prints k in the same format used by wireguard-go.
func (k NodePublic) WireGuardGoString() string {
	// This implementation deliberately matches the overly complicated
	// implementation in wireguard-go.
	b64 := func(input byte) byte {
		return input + 'A' + byte(((25-int(input))>>8)&6) - byte(((51-int(input))>>8)&75) - byte(((61-int(input))>>8)&15) + byte(((62-int(input))>>8)&3)
	}
	b := []byte("peer(____…____)")
	const first = len("peer(")
	const second = len("peer(____…")
	b[first+0] = b64((k.k[0] >> 2) & 63)
	b[first+1] = b64(((k.k[0] << 4) | (k.k[1] >> 4)) & 63)
	b[first+2] = b64(((k.k[1] << 2) | (k.k[2] >> 6)) & 63)
	b[first+3] = b64(k.k[2] & 63)
	b[second+0] = b64(k.k[29] & 63)
	b[second+1] = b64((k.k[30] >> 2) & 63)
	b[second+2] = b64(((k.k[30] << 4) | (k.k[31] >> 4)) & 63)
	b[second+3] = b64((k.k[31] << 2) & 63)
	return string(b)
}
