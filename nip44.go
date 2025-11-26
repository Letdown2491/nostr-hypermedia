package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"golang.org/x/crypto/chacha20"
	"golang.org/x/crypto/hkdf"
)

// NIP-44 version 2 encryption/decryption

const (
	nip44Version       = 2
	nip44Salt          = "nip44-v2"
	minPlaintextSize   = 1
	maxPlaintextSize   = 65535
	minPaddedSize      = 32
)

// GeneratePrivateKey generates a new random secp256k1 private key
func GeneratePrivateKey() ([]byte, error) {
	privKey, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}
	return privKey.Serialize(), nil
}

// GetPublicKey derives the public key from a private key (x-only, 32 bytes)
func GetPublicKey(privKeyBytes []byte) ([]byte, error) {
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	pubKey := privKey.PubKey()
	// Return x-only pubkey (32 bytes) - BIP-340 format
	return pubKey.SerializeCompressed()[1:], nil
}

// GetConversationKey calculates the shared secret between two parties using ECDH
func GetConversationKey(privKeyBytes []byte, pubKeyBytes []byte) ([]byte, error) {
	// Parse private key
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)

	// Parse public key (add 0x02 prefix for even y-coordinate, standard for x-only keys)
	pubKeyWithPrefix := append([]byte{0x02}, pubKeyBytes...)
	pubKey, err := btcec.ParsePubKey(pubKeyWithPrefix)
	if err != nil {
		// Try with 0x03 prefix (odd y-coordinate)
		pubKeyWithPrefix[0] = 0x03
		pubKey, err = btcec.ParsePubKey(pubKeyWithPrefix)
		if err != nil {
			return nil, errors.New("invalid public key")
		}
	}

	// ECDH: multiply pubkey by privkey scalar
	sharedX, _ := pubKey.ToECDSA().Curve.ScalarMult(pubKey.X(), pubKey.Y(), privKey.Serialize())

	// Pad to 32 bytes
	sharedXBytes := make([]byte, 32)
	sharedXBytesRaw := sharedX.Bytes()
	copy(sharedXBytes[32-len(sharedXBytesRaw):], sharedXBytesRaw)

	// HKDF extract with salt "nip44-v2"
	hkdfExtract := hkdf.Extract(sha256.New, sharedXBytes, []byte(nip44Salt))

	return hkdfExtract, nil
}

// getMessageKeys derives ChaCha20 key, nonce, and HMAC key from conversation key and nonce
func getMessageKeys(conversationKey []byte, nonce []byte) (chachaKey, chachaNonce, hmacKey []byte, err error) {
	if len(conversationKey) != 32 {
		return nil, nil, nil, errors.New("invalid conversation key length")
	}
	if len(nonce) != 32 {
		return nil, nil, nil, errors.New("invalid nonce length")
	}

	// HKDF expand
	reader := hkdf.Expand(sha256.New, conversationKey, nonce)
	keys := make([]byte, 76)
	if _, err := reader.Read(keys); err != nil {
		return nil, nil, nil, err
	}

	return keys[0:32], keys[32:44], keys[44:76], nil
}

// calcPaddedLen calculates the padded length for a given plaintext length
func calcPaddedLen(unpaddedLen int) int {
	if unpaddedLen <= 32 {
		return 32
	}

	nextPower := 1 << int(math.Floor(math.Log2(float64(unpaddedLen-1)))+1)
	var chunk int
	if nextPower <= 256 {
		chunk = 32
	} else {
		chunk = nextPower / 8
	}

	return chunk * (int(math.Floor(float64(unpaddedLen-1)/float64(chunk))) + 1)
}

// pad adds NIP-44 padding to plaintext
func pad(plaintext []byte) ([]byte, error) {
	unpaddedLen := len(plaintext)
	if unpaddedLen < minPlaintextSize || unpaddedLen > maxPlaintextSize {
		return nil, errors.New("invalid plaintext length")
	}

	paddedLen := calcPaddedLen(unpaddedLen)
	result := make([]byte, 2+paddedLen)

	// Big-endian length prefix
	binary.BigEndian.PutUint16(result[0:2], uint16(unpaddedLen))
	copy(result[2:], plaintext)
	// Rest is already zero-filled

	return result, nil
}

// unpad removes NIP-44 padding from decrypted data
func unpad(padded []byte) ([]byte, error) {
	if len(padded) < 2 {
		return nil, errors.New("padded data too short")
	}

	unpaddedLen := int(binary.BigEndian.Uint16(padded[0:2]))
	if unpaddedLen == 0 || unpaddedLen > len(padded)-2 {
		return nil, errors.New("invalid padding")
	}

	expectedPaddedLen := calcPaddedLen(unpaddedLen)
	if len(padded) != 2+expectedPaddedLen {
		return nil, errors.New("invalid padded length")
	}

	return padded[2 : 2+unpaddedLen], nil
}

// hmacAAD computes HMAC-SHA256 with additional authenticated data
func hmacAAD(key, message, aad []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(aad)
	h.Write(message)
	return h.Sum(nil)
}

// Nip44Encrypt encrypts plaintext using NIP-44 version 2
func Nip44Encrypt(plaintext string, conversationKey []byte) (string, error) {
	// Generate random nonce
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	return Nip44EncryptWithNonce(plaintext, conversationKey, nonce)
}

// Nip44EncryptWithNonce encrypts with a specific nonce (for testing)
func Nip44EncryptWithNonce(plaintext string, conversationKey []byte, nonce []byte) (string, error) {
	// Get message keys
	chachaKey, chachaNonce, hmacKey, err := getMessageKeys(conversationKey, nonce)
	if err != nil {
		return "", err
	}

	// Pad plaintext
	padded, err := pad([]byte(plaintext))
	if err != nil {
		return "", err
	}

	// Encrypt with ChaCha20
	cipher, err := chacha20.NewUnauthenticatedCipher(chachaKey, chachaNonce)
	if err != nil {
		return "", err
	}
	ciphertext := make([]byte, len(padded))
	cipher.XORKeyStream(ciphertext, padded)

	// Calculate MAC
	mac := hmacAAD(hmacKey, ciphertext, nonce)

	// Concatenate: version || nonce || ciphertext || mac
	result := make([]byte, 1+32+len(ciphertext)+32)
	result[0] = nip44Version
	copy(result[1:33], nonce)
	copy(result[33:33+len(ciphertext)], ciphertext)
	copy(result[33+len(ciphertext):], mac)

	return base64.StdEncoding.EncodeToString(result), nil
}

// Nip44Decrypt decrypts a NIP-44 encrypted payload
func Nip44Decrypt(payload string, conversationKey []byte) (string, error) {
	// Check for future version indicator
	if len(payload) > 0 && payload[0] == '#' {
		return "", errors.New("unsupported encryption version")
	}

	// Base64 decode
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("invalid base64")
	}

	// Validate length
	if len(data) < 99 || len(data) > 65603 {
		return "", errors.New("invalid payload size")
	}

	// Parse components
	version := data[0]
	if version != nip44Version {
		return "", errors.New("unknown version")
	}

	nonce := data[1:33]
	ciphertext := data[33 : len(data)-32]
	mac := data[len(data)-32:]

	// Get message keys
	chachaKey, chachaNonce, hmacKey, err := getMessageKeys(conversationKey, nonce)
	if err != nil {
		return "", err
	}

	// Verify MAC
	calculatedMAC := hmacAAD(hmacKey, ciphertext, nonce)
	if !hmac.Equal(calculatedMAC, mac) {
		return "", errors.New("invalid MAC")
	}

	// Decrypt with ChaCha20
	cipher, err := chacha20.NewUnauthenticatedCipher(chachaKey, chachaNonce)
	if err != nil {
		return "", err
	}
	padded := make([]byte, len(ciphertext))
	cipher.XORKeyStream(padded, ciphertext)

	// Remove padding
	plaintext, err := unpad(padded)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
