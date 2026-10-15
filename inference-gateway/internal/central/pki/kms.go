package pki

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// KMSAPI is the subset of the KMS client the signer uses.
type KMSAPI interface {
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, opts ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
	Sign(ctx context.Context, in *kms.SignInput, opts ...func(*kms.Options)) (*kms.SignOutput, error)
}

// signTimeout bounds one KMS signature.
const signTimeout = 10 * time.Second

// KMSSigner is a crypto.Signer whose ECDSA P-256 key never leaves KMS.
type KMSSigner struct {
	client KMSAPI
	keyID  string
	public *ecdsa.PublicKey
}

// NewKMSSigner fetches the key's public half once.
func NewKMSSigner(ctx context.Context, client KMSAPI, keyID string) (*KMSSigner, error) {
	if keyID == "" {
		return nil, errors.New("pki: CENTRAL_KMS_KEY_ID is required")
	}
	out, err := client.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("pki: KMS GetPublicKey: %w", err)
	}
	if out.KeySpec != types.KeySpecEccNistP256 {
		return nil, fmt.Errorf("pki: KMS key spec %s, want ECC_NIST_P256", out.KeySpec)
	}
	parsed, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("pki: parse KMS public key: %w", err)
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("pki: KMS public key is not ECDSA")
	}
	return &KMSSigner{client: client, keyID: keyID, public: pub}, nil
}

// Public returns the key's public half.
func (s *KMSSigner) Public() crypto.PublicKey { return s.public }

// Sign signs a SHA-256 digest in KMS. The signature is ASN.1 DER, which is
// what crypto/x509 expects from an ECDSA signer.
func (s *KMSSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts.HashFunc() != crypto.SHA256 {
		return nil, fmt.Errorf("pki: KMS signer supports SHA-256 only, got %v", opts.HashFunc())
	}
	ctx, cancel := context.WithTimeout(context.Background(), signTimeout)
	defer cancel()
	out, err := s.client.Sign(ctx, &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          digest,
		MessageType:      types.MessageTypeDigest,
		SigningAlgorithm: types.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("pki: KMS Sign: %w", err)
	}
	return out.Signature, nil
}
