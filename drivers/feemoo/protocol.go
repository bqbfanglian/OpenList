package feemoo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
)

func (d *Feemoo) loadProtocolKeys() error {
	if d.requestKey == nil {
		der, err := base64.StdEncoding.DecodeString(requestPublicKeyDER)
		if err != nil {
			return errors.New("feemoo: invalid web protocol public key")
		}
		key, err := x509.ParsePKIXPublicKey(der)
		if err != nil {
			return err
		}
		var ok bool
		d.requestKey, ok = key.(*rsa.PublicKey)
		if !ok {
			return errors.New("feemoo: web protocol key is not RSA")
		}
	}
	if d.responseKey == nil {
		der, err := base64.StdEncoding.DecodeString(responseClientKeyDER)
		if err != nil {
			return errors.New("feemoo: invalid web protocol response key")
		}
		key, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return err
		}
		var ok bool
		d.responseKey, ok = key.(*rsa.PrivateKey)
		if !ok {
			return errors.New("feemoo: web response key is not RSA")
		}
	}
	return nil
}

// This reproduces the published web-client envelope. It is not account login.
func webCipher(code string) (cipher.Block, []byte, error) {
	digest := md5.Sum([]byte(code))
	hexDigest := hex.EncodeToString(digest[:])
	block, err := aes.NewCipher([]byte(hexDigest[16:]))
	return block, []byte(hexDigest[:16]), err
}

func encryptSecret(data []byte, code string) (string, error) {
	block, iv, err := webCipher(code)
	if err != nil {
		return "", err
	}
	padding := aes.BlockSize - len(data)%aes.BlockSize
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(padded, padded)
	return base64.StdEncoding.EncodeToString(padded), nil
}

func decryptSecret(encoded, code string) ([]byte, error) {
	block, iv, err := webCipher(code)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("feemoo: malformed encrypted response")
	}
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
	padding := int(data[len(data)-1])
	if padding < 1 || padding > aes.BlockSize {
		return nil, errors.New("feemoo: invalid response padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("feemoo: invalid response padding")
		}
	}
	return data[:len(data)-padding], nil
}

func (d *Feemoo) encryptRequest(payload any) (map[string]string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	code := hex.EncodeToString(random)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sn, err := rsa.EncryptPKCS1v15(rand.Reader, d.requestKey, []byte(code))
	if err != nil {
		return nil, errors.New("feemoo: cannot encode web request")
	}
	jt, err := encryptSecret(data, code)
	if err != nil {
		return nil, err
	}
	return map[string]string{"sn": base64.StdEncoding.EncodeToString(sn), "jt": jt}, nil
}

func (d *Feemoo) decryptResponse(data json.RawMessage) ([]byte, error) {
	var encrypted struct {
		AK string `json:"ak"`
		ED string `json:"ed"`
	}
	if len(data) == 0 {
		return nil, errors.New("feemoo: missing API response data")
	}
	if data[0] != '{' {
		return data, nil
	}
	if err := json.Unmarshal(data, &encrypted); err != nil {
		return nil, errors.New("feemoo: invalid response envelope")
	}
	if encrypted.AK == "" {
		return data, nil
	}
	ak, err := base64.StdEncoding.DecodeString(encrypted.AK)
	if err != nil {
		return nil, errors.New("feemoo: invalid response key encoding")
	}
	code, err := rsa.DecryptPKCS1v15(rand.Reader, d.responseKey, ak)
	if err != nil {
		return nil, errors.New("feemoo: web protocol response key has changed")
	}
	return decryptSecret(encrypted.ED, string(code))
}
