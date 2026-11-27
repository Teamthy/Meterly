package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const coseAlgES256 = -7

func b64u(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64uDecode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("identity: invalid base64")
}

// parseECPoint accepts X||Y (64), uncompressed 0x04||X||Y (65), SPKI, or base64 of those.
func parseECPoint(s string) ([]byte, error) {
	b, err := b64uDecode(s)
	if err != nil {
		b = []byte(s)
	}
	switch {
	case len(b) == 64:
		return b, nil
	case len(b) == 65 && b[0] == 0x04:
		return b[1:], nil
	case len(b) >= 65 && b[len(b)-65] == 0x04:
		return b[len(b)-64:], nil
	default:
		return nil, ErrBadAttestation
	}
}

type clientData struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	Origin    string `json:"origin"`
}

func parseClientData(raw []byte, wantType, wantOrigin string, challenge []byte) error {
	var cd clientData
	if err := json.Unmarshal(raw, &cd); err != nil {
		return ErrBadAttestation
	}
	if cd.Type != wantType {
		return fmt.Errorf("%w: client data type", ErrBadAttestation)
	}
	got, err := b64uDecode(cd.Challenge)
	if err != nil || len(got) != len(challenge) {
		return ErrChallenge
	}
	if subtleEqual(got, challenge) != 1 {
		return ErrChallenge
	}
	if wantOrigin != "" && cd.Origin != wantOrigin {
		return ErrOrigin
	}
	return nil
}

func rpIDHash(rpID string) []byte {
	sum := sha256.Sum256([]byte(rpID))
	return sum[:]
}

type attested struct {
	CredID    []byte
	PublicKey []byte // X||Y
	AAGUID    []byte
	SignCount uint32
}

func parseAttested(authData []byte, rpID string) (attested, error) {
	var zero attested
	if len(authData) < 37 {
		return zero, ErrBadAttestation
	}
	if subtleEqual(authData[:32], rpIDHash(rpID)) != 1 {
		return zero, ErrOrigin
	}
	flags := authData[32]
	sc := binary.BigEndian.Uint32(authData[33:37])
	if flags&0x40 == 0 {
		return zero, ErrBadAttestation
	}
	rest := authData[37:]
	if len(rest) < 18 {
		return zero, ErrBadAttestation
	}
	aaguid := append([]byte(nil), rest[:16]...)
	credLen := binary.BigEndian.Uint16(rest[16:18])
	rest = rest[18:]
	if int(credLen) > len(rest) || credLen == 0 {
		return zero, ErrBadAttestation
	}
	credID := append([]byte(nil), rest[:credLen]...)
	cose := rest[credLen:]
	x, y, err := parseCOSEECKey(cose)
	if err != nil {
		return zero, err
	}
	pk := append(x, y...)
	return attested{CredID: credID, PublicKey: pk, AAGUID: aaguid, SignCount: sc}, nil
}

func parseAssertionAuthData(authData []byte, rpID string) (uint32, error) {
	if len(authData) < 37 {
		return 0, ErrBadAssertion
	}
	if subtleEqual(authData[:32], rpIDHash(rpID)) != 1 {
		return 0, ErrOrigin
	}
	// Options request userVerification=required: demand both user present
	// (0x01) and user verified (0x04), so a touch without PIN/biometric
	// cannot sign in or satisfy a step-up.
	if authData[32]&0x01 == 0 || authData[32]&0x04 == 0 {
		return 0, ErrBadAssertion
	}
	return binary.BigEndian.Uint32(authData[33:37]), nil
}

func verifyES256(pubXY, authData, clientDataJSON, sig []byte) error {
	if len(pubXY) != 64 || len(sig) < 64 {
		return ErrBadAssertion
	}
	cdh := sha256.Sum256(clientDataJSON)
	msg := append(append([]byte{}, authData...), cdh[:]...)
	digest := sha256.Sum256(msg)
	x := new(big.Int).SetBytes(pubXY[:32])
	y := new(big.Int).SetBytes(pubXY[32:])
	pub := ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	var r, s *big.Int
	if len(sig) == 64 {
		r = new(big.Int).SetBytes(sig[:32])
		s = new(big.Int).SetBytes(sig[32:])
	} else {
		// DER
		if !ecdsa.VerifyASN1(&pub, digest[:], sig) {
			return ErrBadAssertion
		}
		return nil
	}
	if !ecdsa.Verify(&pub, digest[:], r, s) {
		return ErrBadAssertion
	}
	return nil
}

func parseCOSEECKey(b []byte) (x, y []byte, err error) {
	m, _, err := decodeCBORMap(b)
	if err != nil {
		return nil, nil, ErrBadAttestation
	}
	kty, _ := m[1].(int64)
	alg, _ := m[3].(int64)
	crv, _ := m[-1].(int64)
	xb, _ := m[-2].([]byte)
	yb, _ := m[-3].([]byte)
	if kty != 2 || alg != coseAlgES256 || crv != 1 || len(xb) != 32 || len(yb) != 32 {
		return nil, nil, ErrBadAttestation
	}
	return xb, yb, nil
}

func subtleEqual(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	if v == 0 {
		return 1
	}
	return 0
}

// Minimal CBOR map decoder for COSE_Key (integer keys, bstr/int values).
func decodeCBORMap(b []byte) (map[int64]any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errors.New("cbor empty")
	}
	mt, n, rest, err := cborHead(b)
	if err != nil {
		return nil, nil, err
	}
	if mt != 5 {
		return nil, nil, errors.New("cbor: not a map")
	}
	out := map[int64]any{}
	for i := uint64(0); i < n; i++ {
		k, r2, err := cborInt(rest)
		if err != nil {
			return nil, nil, err
		}
		v, r3, err := cborVal(r2)
		if err != nil {
			return nil, nil, err
		}
		out[k] = v
		rest = r3
	}
	return out, rest, nil
}

func cborHead(b []byte) (major byte, n uint64, rest []byte, err error) {
	if len(b) == 0 {
		return 0, 0, nil, errors.New("cbor truncated")
	}
	major = b[0] >> 5
	ai := b[0] & 0x1f
	rest = b[1:]
	switch {
	case ai < 24:
		return major, uint64(ai), rest, nil
	case ai == 24:
		if len(rest) < 1 {
			return 0, 0, nil, errors.New("cbor truncated")
		}
		return major, uint64(rest[0]), rest[1:], nil
	case ai == 25:
		if len(rest) < 2 {
			return 0, 0, nil, errors.New("cbor truncated")
		}
		return major, uint64(binary.BigEndian.Uint16(rest[:2])), rest[2:], nil
	case ai == 26:
		if len(rest) < 4 {
			return 0, 0, nil, errors.New("cbor truncated")
		}
		return major, uint64(binary.BigEndian.Uint32(rest[:4])), rest[4:], nil
	default:
		return 0, 0, nil, errors.New("cbor additional info")
	}
}

func cborInt(b []byte) (int64, []byte, error) {
	mt, n, rest, err := cborHead(b)
	if err != nil {
		return 0, nil, err
	}
	switch mt {
	case 0:
		return int64(n), rest, nil
	case 1:
		return -1 - int64(n), rest, nil
	default:
		return 0, nil, errors.New("cbor: expected int")
	}
}

func cborVal(b []byte) (any, []byte, error) {
	mt, n, rest, err := cborHead(b)
	if err != nil {
		return nil, nil, err
	}
	switch mt {
	case 0:
		return int64(n), rest, nil
	case 1:
		return -1 - int64(n), rest, nil
	case 2:
		if uint64(len(rest)) < n {
			return nil, nil, errors.New("cbor truncated bstr")
		}
		return rest[:n], rest[n:], nil
	default:
		return nil, nil, errors.New("cbor unsupported value")
	}
}
