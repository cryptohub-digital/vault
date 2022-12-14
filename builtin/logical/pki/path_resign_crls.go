package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/certutil"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	crlNumberParam          = "crl_number"
	deltaCrlBaseNumberParam = "delta_crl_base_number"
	nextUpdateParam         = "next_update"
	crlsParam               = "crls"
	formatParam             = "format"
)

func pathResignCrls(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "issuer/" + framework.GenericNameRegex(issuerRefParam) + "/resign-crls",
		Fields: map[string]*framework.FieldSchema{
			issuerRefParam: {
				Type: framework.TypeString,
				Description: `Reference to a existing issuer; either "default"
for the configured default issuer, an identifier or the name assigned
to the issuer.`,
				Default: defaultRef,
			},
			crlNumberParam: {
				Type:        framework.TypeInt,
				Description: `The sequence number to be written within the CRL Number extension.`,
			},
			deltaCrlBaseNumberParam: {
				Type: framework.TypeInt,
				Description: `Using a zero or greater value specifies the base CRL revision number to encode within
 a Delta CRL indicator extension, otherwise the extension will not be added.`,
				Default: -1,
			},
			nextUpdateParam: {
				Type: framework.TypeString,
				Description: `The amount of time the generated CRL should be
valid; defaults to 72 hours.`,
				Default: defaultCrlConfig.Expiry,
			},
			crlsParam: {
				Type:        framework.TypeStringSlice,
				Description: `A list of PEM encoded CRLs to combine, originally signed by the requested issuer.`,
			},
			formatParam: {
				Type: framework.TypeString,
				Description: `The format of the combined CRL, can be "pem" or "der". If "der", the value will be
base64 encoded. Defaults to "pem".`,
				Default: "pem",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathUpdateResignCrlsHandler,
			},
		},

		HelpSynopsis: `Combine and sign with the provided issuer different CRLs`,
		HelpDescription: `Provide two or more PEM encoded CRLs signed by the issuer,
 normally from separate Vault clusters to be combined and signed.`,
	}
}

func (b *backend) pathUpdateResignCrlsHandler(ctx context.Context, request *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	if b.useLegacyBundleCaStorage() {
		return logical.ErrorResponse("This API cannot be used until the migration has completed"), nil
	}

	issuerRef := getIssuerRef(data)
	crlNumber := data.Get(crlNumberParam).(int)
	deltaCrlBaseNumber := data.Get(deltaCrlBaseNumberParam).(int)
	nextUpdateStr := data.Get(nextUpdateParam).(string)
	rawCrls := data.Get(crlsParam).([]string)

	format, err := getCrlFormat(data.Get(formatParam).(string))
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	nextUpdateOffset, err := time.ParseDuration(nextUpdateStr)
	if err != nil {
		return logical.ErrorResponse("invalid value for %s: %v", nextUpdateParam, err), nil
	}

	if nextUpdateOffset <= 0 {
		return logical.ErrorResponse("%s parameter must be greater than 0", nextUpdateParam), nil
	}

	if crlNumber < 0 {
		return logical.ErrorResponse("%s parameter must be 0 or greater", crlNumberParam), nil
	}
	if deltaCrlBaseNumber < -1 {
		return logical.ErrorResponse("%s parameter must be -1 or greater", deltaCrlBaseNumberParam), nil
	}

	if issuerRef == "" {
		return logical.ErrorResponse("%s parameter cannot be blank", issuerRefParam), nil
	}

	providedCrls, err := decodePemCrls(rawCrls)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	sc := b.makeStorageContext(ctx, request.Storage)
	caBundle, err := getCaBundle(sc, issuerRef)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	if err := verifyCrlsAreFromIssuersKey(caBundle.Certificate, providedCrls); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	revokedCerts, warnings, err := getAllRevokedCerts(providedCrls)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	now := time.Now()
	template := &x509.RevocationList{
		SignatureAlgorithm:  caBundle.RevocationSigAlg,
		RevokedCertificates: revokedCerts,
		Number:              big.NewInt(int64(crlNumber)),
		ThisUpdate:          now,
		NextUpdate:          now.Add(nextUpdateOffset),
	}

	if deltaCrlBaseNumber > -1 {
		ext, err := certutil.CreateDeltaCRLIndicatorExt(int64(deltaCrlBaseNumber))
		if err != nil {
			return nil, fmt.Errorf("could not create crl delta indicator extension: %v", err)
		}
		template.ExtraExtensions = []pkix.Extension{ext}
	}

	crlBytes, err := x509.CreateRevocationList(rand.Reader, template, caBundle.Certificate, caBundle.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("error creating new CRL: %w", err)
	}

	body := encodeResponse(crlBytes, format == "der")

	return &logical.Response{
		Warnings: warnings,
		Data: map[string]interface{}{
			"crl": body,
		},
	}, nil
}

func verifyCrlsAreFromIssuersKey(caCert *x509.Certificate, crls []*x509.RevocationList) error {
	for i, crl := range crls {
		// At this point we assume if the issuer's key signed the CRL that is a good enough check
		// to validate that we owned/generated the provided CRL.
		if err := crl.CheckSignatureFrom(caCert); err != nil {
			return fmt.Errorf("CRL index: %d was not signed by requested issuer", i)
		}
	}

	return nil
}

func encodeResponse(crlBytes []byte, derFormatRequested bool) string {
	if derFormatRequested {
		return base64.StdEncoding.EncodeToString(crlBytes)
	}

	block := pem.Block{
		Type:  "X509 CRL",
		Bytes: crlBytes,
	}
	return string(pem.EncodeToMemory(&block))
}

func getCrlFormat(requestedValue string) (string, error) {
	format := strings.ToLower(requestedValue)
	switch format {
	case "pem", "der":
		return format, nil
	default:
		return "", fmt.Errorf("unknown format value of %s", requestedValue)
	}
}

func getAllRevokedCerts(crls []*x509.RevocationList) ([]pkix.RevokedCertificate, []string, error) {
	uniqueCert := map[string]pkix.RevokedCertificate{}
	var warnings []string
	for _, crl := range crls {
		for _, curCert := range crl.RevokedCertificates {
			serial := serialFromBigInt(curCert.SerialNumber)
			// Get rid of any extensions the existing certificate might have had.
			curCert.Extensions = []pkix.Extension{}

			existingCert, exists := uniqueCert[serial]
			if !exists {
				// First time we see the revoked cert
				uniqueCert[serial] = curCert
				continue
			}

			if existingCert.RevocationTime.Equal(curCert.RevocationTime) {
				// Same revocation times, just skip it
				continue
			}

			warn := fmt.Sprintf("Duplicate serial %s with different revocation "+
				"times detected, using oldest revocation time", serial)
			warnings = append(warnings, warn)

			if existingCert.RevocationTime.After(curCert.RevocationTime) {
				uniqueCert[serial] = curCert
			}
		}
	}

	var revokedCerts []pkix.RevokedCertificate
	for _, cert := range uniqueCert {
		revokedCerts = append(revokedCerts, cert)
	}

	return revokedCerts, warnings, nil
}

func getCaBundle(sc *storageContext, issuerRef string) (*certutil.CAInfoBundle, error) {
	issuerId, err := sc.resolveIssuerReference(issuerRef)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve issuer %s: %w", issuerRefParam, err)
	}

	return sc.fetchCAInfoByIssuerId(issuerId, CRLSigningUsage)
}

func decodePemCrls(rawCrls []string) ([]*x509.RevocationList, error) {
	var crls []*x509.RevocationList
	for i, rawCrl := range rawCrls {
		crl, err := decodePemCrl(rawCrl)
		if err != nil {
			return nil, fmt.Errorf("failed decoding crl %d: %w", i, err)
		}
		crls = append(crls, crl)
	}

	return crls, nil
}

func decodePemCrl(crl string) (*x509.RevocationList, error) {
	block, rest := pem.Decode([]byte(crl))
	if len(rest) != 0 {
		return nil, errors.New("invalid crl; should be one PEM block only")
	}

	return x509.ParseRevocationList(block.Bytes)
}
