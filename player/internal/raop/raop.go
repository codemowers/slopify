// Package raop is a playback-only AirPlay 1 (RAOP) receiver: RTSP session control
// and ALAC over RTP/UDP. It has no authentication, so it belongs on a trusted LAN.
//
// Audio always reaches the output through Stream, which knows nothing of how a
// session was negotiated, and packets pass through a packetCipher before decoding.
// AirPlay 2 would add its own pairing, timing and ciphers around those two seams
// rather than changing them. It is not implemented, and neither is video.
//
// Encryption is optional. Without a key the receiver offers plain RAOP, which
// PipeWire and PulseAudio can send. Apple devices require RSA, whose private key
// is deliberately not part of this repository: see LoadKey.
package raop

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
)

// DefaultBufferMillis is the playout delay unless configured otherwise.
const DefaultBufferMillis = 300

// ServiceType is the mDNS service senders browse for.
const ServiceType = "_raop._tcp"

// publicModulus is the RSA public key every RAOP sender encrypts to, as published
// in PipeWire's module-raop-sink.
const publicModulus = "59dE8qLieItsH1WgjrcFRKj6eUWqi+bGLOX1HL3U3GhC/j0Qg90u3sG/1CUtwC" +
	"5vOYvfDmFI6oSFXi5ELabWJmT2dKHzBJKa3k9ok+8t9ucRqMd6DZHJ2YCCLlDR" +
	"KSKv6kDqnw4UwPdpOMXziC/AMj3Z/lUVX1G7WSHCAWKf1zNS1eLvqr+boEjXuB" +
	"OitnZ/bDzPHrTOZz0Dew0uowxf/+sG+NCK3eQJVxqcaJ/vEHKIVd2M+5qL71yJ" +
	"Q+87X6oV3eaYvt3zWZYD6z5vYTcrtij2VZ9Zmni/UAaHqn9JdsBWLUEpVviYnh" +
	"imNVvYFZeCXg/IdTQ+x4IRdiXNv5hEew=="

// LoadKey reads a PEM RSA private key and accepts it only if it is the key senders
// encrypt to. The match makes the file's origin irrelevant: anything else is useless
// to a sender and is refused, so an untrusted download cannot substitute its own.
func LoadKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("raop key: no PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("raop key: %w", err)
	}
	want, err := base64.StdEncoding.DecodeString(publicModulus)
	if err != nil {
		return nil, err
	}
	if key.N.Cmp(new(big.Int).SetBytes(want)) != 0 {
		return nil, errors.New("raop key: not the RAOP key")
	}
	return key, nil
}

// HardwareID derives the stable address a receiver identifies itself by. It is an
// arbitrary label, not a real interface address.
func HardwareID(seed string) [6]byte {
	sum := sha256.Sum256([]byte("slopify-raop:" + seed))
	var id [6]byte
	copy(id[:], sum[:])
	id[0] = id[0]&^1 | 2 // Locally administered, unicast.
	return id
}

// Advertisement returns the mDNS instance name and TXT records. Offering et=1 makes
// PipeWire choose RSA, and it is what Apple devices need. md=0 asks for track text only.
func Advertisement(name string, id [6]byte, encrypted bool) (string, []string) {
	et := "et=0"
	txt := []string{"txtvers=1", "tp=UDP", "ch=2", "cn=0,1", "ss=16", "sr=44100", "pw=false",
		"sm=false", "sv=false", "da=true", "vn=3", "vs=105.1", "sf=0x4", "am=Slopify", "md=0"}
	if encrypted {
		et = "et=0,1"
		txt = append(txt, "ek=1")
	}
	return fmt.Sprintf("%X@%s", id[:], name), append(txt, et)
}

// packetCipher turns one packet's audio back into plaintext, in place.
type packetCipher interface{ decrypt([]byte) }

// aesCBC is RAOP's scheme: each packet restarts from the session's IV, and a trailing
// partial block is sent in the clear.
type aesCBC struct {
	block cipher.Block
	iv    []byte
}

func (c *aesCBC) decrypt(data []byte) {
	n := len(data) &^ (aes.BlockSize - 1)
	if n > 0 {
		cipher.NewCBCDecrypter(c.block, c.iv).CryptBlocks(data[:n], data[:n])
	}
}

func unpaddedBase64(s string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(s), "="))
}

func newAESCBC(key *rsa.PrivateKey, wrapped, iv string) (packetCipher, error) {
	if key == nil {
		return nil, errors.New("encrypted session without a configured key")
	}
	ct, err := unpaddedBase64(wrapped)
	if err != nil {
		return nil, err
	}
	aesKey, err := rsa.DecryptOAEP(sha1.New(), nil, key, ct, nil)
	if err != nil {
		return nil, err
	}
	ivBytes, err := unpaddedBase64(iv)
	if err != nil {
		return nil, err
	}
	if len(aesKey) != 16 || len(ivBytes) != aes.BlockSize {
		return nil, errors.New("wrong AES key or IV size")
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	return &aesCBC{block: block, iv: ivBytes}, nil
}

// appleResponse proves possession of the key: a raw PKCS#1 signature over the
// challenge, the address the sender reached us on and our hardware ID.
func appleResponse(key *rsa.PrivateKey, challenge string, ip []byte, id [6]byte) (string, error) {
	data, err := unpaddedBase64(challenge)
	if err != nil {
		return "", err
	}
	if len(data) > 64 {
		return "", errors.New("oversized challenge")
	}
	data = append(append(data, ip...), id[:]...)
	for len(data) < 32 {
		data = append(data, 0)
	}
	signature, err := rsa.SignPKCS1v15(nil, key, crypto.Hash(0), data)
	if err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(signature), nil
}
