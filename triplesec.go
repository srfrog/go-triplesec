// The design and name of TripleSec is (C) Keybase 2013
// This Go implementation is (C) Filippo Valsorda 2014
// Use of this source code is governed by the MIT License

// Package triplesec implements the TripleSec v3 encryption and authentication scheme.
//
// For details on TripleSec, go to https://keybase.io/triplesec/
package triplesec

import (
	"bytes"
	"code.google.com/p/go.crypto/salsa20"
	"code.google.com/p/go.crypto/scrypt"
	"github.com/keybase/go-triplesec/sha3"
	"code.google.com/p/go.crypto/twofish"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"fmt"
)

const SaltSize = 16

type Cipher struct {
	passphrase []byte
	salt       []byte
	derivedKey []byte
}

// A Cipher is an instance of TripleSec using a particular key and
// a particular salt
func NewCipher(passphrase []byte, salt []byte) (*Cipher, error) {
	if salt != nil && len(salt) != SaltSize {
		return nil, fmt.Errorf("Need a salt of size %d", SaltSize)
	}
	return &Cipher{passphrase, salt, nil}, nil
}

func (c *Cipher) GetSalt() ([]byte, error) {
	if c.salt != nil {
		return c.salt, nil
	}
	c.salt = make([]byte, SaltSize)
	_, err := rand.Read(c.salt)
	if err != nil {
		return nil, err
	}
	return c.salt, nil
}

func (c *Cipher) DeriveKey(dkLen int) ([]byte, []byte, error) {

	if dkLen < DkSize {
		dkLen = DkSize
	}

	if c.derivedKey == nil || len(c.derivedKey) < dkLen {
		dk, err := scrypt.Key(c.passphrase, c.salt, 32768, 8, 1, dkLen)
		if err != nil {
			return nil, nil, err
		}
		c.derivedKey = dk
	}
	return c.derivedKey[0:DkSize], c.derivedKey[DkSize:], nil
}

// The MagicBytes are the four bytes prefixed to every TripleSec
// ciphertext, 1c 94 d7 de.
var MagicBytes = [4]byte{ 0x1c, 0x94, 0xd7, 0xde }

var Version uint32 = 3
const MacOutputLen = 64

var (
	macKeyLen    = 48
	cipherKeyLen = 32
	DkSize       = 2*macKeyLen + 3*cipherKeyLen
)

// Overhead is the amount of bytes added to a TripleSec ciphertext.
// 	len(plaintext) + Overhead = len(ciphertext)
// It consists of: magic bytes + version + salt + 2 * MACs + 3 * IVS.
const Overhead = len(MagicBytes) + 4 + SaltSize + 2*MacOutputLen + 16 + 16 + 24

// Encrypt encrypts and signs a plaintext message with TripleSec using a random
// salt and the Cipher passphrase. The dst buffer size must be at least len(src)
// + Overhead. dst and src can not overlap. src is left untouched.
//
// Encrypt returns a error on memory or RNG failures.
func (c *Cipher) Encrypt(src []byte) (dst []byte, err error) {
	if len(src) < 1 {
		return nil, fmt.Errorf("the plaintext cannot be empty")
	}

	dst = make([]byte, len(src) + Overhead)
	buf := bytes.NewBuffer(dst[:0])

	_, err = buf.Write(MagicBytes[0:])
	if err != nil {
		return
	}

	// Write version
	err = binary.Write(buf, binary.BigEndian, Version)
	if err != nil {
		return
	}

	salt, err := c.GetSalt()
	if err != nil {
		return
	}

	_, err = buf.Write(salt)
	if err != nil {
		return
	}

	dk, _, err := c.DeriveKey(DkSize)
	if err != nil {
		return
	}
	macKeys := dk[:macKeyLen*2]
	cipherKeys := dk[macKeyLen*2:]

	// The allocation over here can be made better
	encryptedData, err := encrypt_data(src, cipherKeys)
	if err != nil {
		return
	}

	authenticatedData := make([]byte, 0, buf.Len()+len(encryptedData))
	authenticatedData = append(authenticatedData, buf.Bytes()...)
	authenticatedData = append(authenticatedData, encryptedData...)
	macsOutput := generate_macs(authenticatedData, macKeys)

	_, err = buf.Write(macsOutput)
	if err != nil {
		return
	}
	_, err = buf.Write(encryptedData)
	if err != nil {
		return
	}

	if buf.Len() != len(src)+Overhead {
		panic(fmt.Errorf("something went terribly wrong: output size wrong"))
	}

	return buf.Bytes(), nil
}

func encrypt_data(plain, keys []byte) ([]byte, error) {
	var iv, key []byte
	var block cipher.Block
	var stream cipher.Stream

	iv_offset := 16 + 16 + 24
	res := make([]byte, len(plain)+iv_offset)

	iv = res[iv_offset-24 : iv_offset]
	_, err := rand.Read(iv)
	if err != nil {
		return nil, err
	}
	// For some reason salsa20 API is different
	key_array := new([32]byte)
	copy(key_array[:], keys[cipherKeyLen*2:])
	salsa20.XORKeyStream(res[iv_offset:], plain, iv, key_array)
	iv_offset -= 24

	iv = res[iv_offset-16 : iv_offset]
	_, err = rand.Read(iv)
	if err != nil {
		return nil, err
	}
	key = keys[cipherKeyLen : cipherKeyLen*2]
	block, err = twofish.NewCipher(key)
	if err != nil {
		return nil, err
	}
	stream = cipher.NewCTR(block, iv)
	stream.XORKeyStream(res[iv_offset:], res[iv_offset:])
	iv_offset -= 16

	iv = res[iv_offset-16 : iv_offset]
	_, err = rand.Read(iv)
	if err != nil {
		return nil, err
	}
	key = keys[:cipherKeyLen]
	block, err = aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	stream = cipher.NewCTR(block, iv)
	stream.XORKeyStream(res[iv_offset:], res[iv_offset:])
	iv_offset -= 16

	if iv_offset != 0 {
		panic(fmt.Errorf("something went terribly wrong: iv_offset final value non-zero"))
	}

	return res, nil
}

func generate_macs(data, keys []byte) []byte {
	res := make([]byte, 0, 64*2)

	key := keys[:macKeyLen]
	mac := hmac.New(sha512.New, key)
	mac.Write(data)
	res = mac.Sum(res)

	key = keys[macKeyLen:]
	mac = hmac.New(sha3.NewKeccak512, key)
	mac.Write(data)
	res = mac.Sum(res)

	return res
}

// Decrypt decrypts a TripleSec ciphertext using the Cipher passphrase.
// The dst buffer size must be at least len(src) - Overhead.
// dst and src can not overlap. src is left untouched.
//
// Encrypt returns a error if the ciphertext is not recognized, if
// authentication fails or on memory failures.
func (c *Cipher) Decrypt(dst, src []byte) error {
	if len(src) <= Overhead {
		return fmt.Errorf("the ciphertext is too short to be a TripleSec ciphertext")
	}
	if len(dst) < len(src)-Overhead {
		return fmt.Errorf("the dst buffer is too short to hold the plaintext")
	}

	if !bytes.Equal(src[:4], MagicBytes[0:]) {
		return fmt.Errorf("the ciphertext does not look like a TripleSec ciphertext")
	}

	v := make([]byte, 4)
	v_b := bytes.NewBuffer(v[:0])
	err := binary.Write(v_b, binary.BigEndian, uint32(3))
	if err != nil {
		return err
	}
	if !bytes.Equal(src[4:8], v) {
		return fmt.Errorf("unknown version")
	}

	salt := src[8:24]
	dk, err := scrypt.Key(c.passphrase, salt, 32768, 8, 1, DkSize)
	if err != nil {
		return err
	}
	macKeys := dk[:macKeyLen*2]
	cipherKeys := dk[macKeyLen*2:]

	macs := src[24 : 24+64*2]
	encryptedData := src[24+64*2:]

	authenticatedData := make([]byte, 0, 24+len(encryptedData))
	authenticatedData = append(authenticatedData, src[:24]...)
	authenticatedData = append(authenticatedData, encryptedData...)

	if !hmac.Equal(macs, generate_macs(authenticatedData, macKeys)) {
		return fmt.Errorf("TripleSec ciphertext authentication FAILED")
	}

	err = decrypt_data(dst, encryptedData, cipherKeys)
	if err != nil {
		return err
	}

	return nil
}

func decrypt_data(dst, data, keys []byte) error {
	var iv, key []byte
	var block cipher.Block
	var stream cipher.Stream
	var err error

	buffer := append([]byte{}, data...)

	iv_offset := 16
	iv = buffer[:iv_offset]
	key = keys[:cipherKeyLen]
	block, err = aes.NewCipher(key)
	if err != nil {
		return err
	}
	stream = cipher.NewCTR(block, iv)
	stream.XORKeyStream(buffer[iv_offset:], buffer[iv_offset:])

	iv_offset += 16
	iv = buffer[iv_offset-16 : iv_offset]
	key = keys[cipherKeyLen : cipherKeyLen*2]
	block, err = twofish.NewCipher(key)
	if err != nil {
		return err
	}
	stream = cipher.NewCTR(block, iv)
	stream.XORKeyStream(buffer[iv_offset:], buffer[iv_offset:])

	iv_offset += 24
	iv = buffer[iv_offset-24 : iv_offset]
	key_array := new([32]byte)
	copy(key_array[:], keys[cipherKeyLen*2:])
	salsa20.XORKeyStream(dst, buffer[iv_offset:], iv, key_array)

	if len(buffer[iv_offset:]) != len(data)-(16+16+24) {
		panic(fmt.Errorf("something went terribly wrong: buffer size is not consistent"))
	}

	return nil
}
