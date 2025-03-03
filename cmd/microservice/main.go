package main

import "C"
import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/arkavo/backend-go/pkg/access"
	"github.com/arkavo/backend-go/pkg/keys"
	"github.com/arkavo/backend-go/pkg/p11"
	"github.com/arkavo/backend-go/pkg/wellknown"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/miekg/pkcs11"
	"golang.org/x/oauth2"
)

const (
	ErrHsm             = Error("hsm unexpected")
	timeoutServerRead  = 5 * time.Second
	timeoutServerWrite = 10 * time.Second
	timeoutServerIdle  = 120 * time.Second
)

var Version string

func main() {
	log.Printf("Version: %s", Version)
	hostname := os.Getenv("SERVER_PUBLIC_NAME")
	kasURI, _ := url.Parse("https://" + hostname + ":5000")
	kas := access.Provider{
		URI:          *kasURI,
		PrivateKey:   p11.Pkcs11PrivateKeyRSA{},
		PublicKeyRsa: rsa.PublicKey{},
		PublicKeyEc:  ecdsa.PublicKey{},
		Certificate:  x509.Certificate{},
		Attributes:   nil,
		Session:      p11.Pkcs11Session{},
		OIDCVerifier: nil,
	}
	// OIDC
	oidcIssuer := os.Getenv("OIDC_ISSUER")
	provider, err := oidc.NewProvider(context.Background(), oidcIssuer)
	if err != nil {
		// handle error
		log.Panic(err)
	}
	// Configure an OpenID Connect aware OAuth2 client.
	oauth2Config := oauth2.Config{
		ClientID:     "",
		ClientSecret: "",
		RedirectURL:  "",
		// Discovery returns the OAuth2 endpoints.
		Endpoint: provider.Endpoint(),
		// "openid" is a required scope for OpenID Connect flows.
		Scopes: []string{oidc.ScopeOpenID},
	}
	log.Println(oauth2Config)
	oidcConfig := oidc.Config{
		ClientID:                   "",
		SupportedSigningAlgs:       nil,
		SkipClientIDCheck:          true,
		SkipExpiryCheck:            false,
		SkipIssuerCheck:            false,
		Now:                        nil,
		InsecureSkipSignatureCheck: false,
	}
	var verifier = provider.Verifier(&oidcConfig)

	kas.OIDCVerifier = verifier

	// PKCS#11
	pin := os.Getenv("PKCS11_PIN")
	rsaLabel := os.Getenv("PKCS11_LABEL_PUBKEY_RSA") // development-rsa-kas
	ecLabel := os.Getenv("PKCS11_LABEL_PUBKEY_EC")   // development-ec-kas
	slot, err := strconv.ParseInt(os.Getenv("PKCS11_SLOT_INDEX"), 10, 32)
	if err != nil {
		log.Fatalf("PKCS11_SLOT parse error: %v", err)
	}
	pkcs11ModulePath := os.Getenv("PKCS11_MODULE_PATH")
	log.Println(pkcs11ModulePath)
	ctx := pkcs11.New(pkcs11ModulePath)
	if err := ctx.Initialize(); err != nil {
		log.Fatalf("error initializing module: %v", err)
	}
	defer ctx.Destroy()
	defer func(ctx *pkcs11.Ctx) {
		err := ctx.Finalize()
		if err != nil {
			log.Println(err)
		}
	}(ctx)
	log.Println(ctx.GetInfo())
	var keyID []byte
	slots, err := ctx.GetSlotList(true)
	if err != nil {
		log.Panicf("error getting slots: %v", err)
	}
	log.Println(slots)
	if int(slot) >= len(slots) || slot < 0 {
		log.Panicf("fail PKCS11_SLOT_INDEX is invalid")
	}
	log.Println(slots[slot])
	session, err := ctx.OpenSession(slots[slot], pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		log.Panicf("error opening session: %v", err)
	}
	defer func(ctx *pkcs11.Ctx, sh pkcs11.SessionHandle) {
		err := ctx.CloseSession(sh)
		if err != nil {
			log.Println(err)
		}
	}(ctx, session)

	err = ctx.Login(session, pkcs11.CKU_USER, pin)
	if err != nil {
		log.Panicf("error logging in: %v", err)
	}
	defer func(ctx *pkcs11.Ctx, sh pkcs11.SessionHandle) {
		err := ctx.Logout(sh)
		if err != nil {
			log.Println(err)
		}
	}(ctx, session)
	log.Println(ctx.GetInfo())
	log.Println("Finding RSA key to wrap.")
	keyHandle, err := findKey(ctx, session, pkcs11.CKO_PRIVATE_KEY, keyID, rsaLabel)
	if err != nil {
		log.Panicf("error finding key: %v", err)
	}
	log.Println(keyHandle)

	// set private key
	kas.PrivateKey = p11.NewPrivateKeyRSA(keyHandle)

	// initialize p11.pkcs11session
	kas.Session = p11.NewSession(ctx, session)

	// RSA Cert
	log.Printf("Finding RSA certificate: %s", rsaLabel)
	certHandle, err := findKey(ctx, session, pkcs11.CKO_CERTIFICATE, keyID, rsaLabel)
	certTemplate := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_CERTIFICATE),
		pkcs11.NewAttribute(pkcs11.CKA_CERTIFICATE_TYPE, pkcs11.CKC_X_509),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE, []byte("")),
		pkcs11.NewAttribute(pkcs11.CKA_SUBJECT, []byte("")),
	}
	if err != nil {
		log.Panic(err)
	}
	attrs, err := ctx.GetAttributeValue(session, certHandle, certTemplate)
	if err != nil {
		log.Panic(err)
	}
	log.Println(attrs)

	for i, a := range attrs {
		log.Printf("attr %d, type %d, valuelen %d\n", i, a.Type, len(a.Value))
		if a.Type == pkcs11.CKA_VALUE {
			certRsa, err := x509.ParseCertificate(a.Value)
			if err != nil {
				log.Panic(err)
			}
			kas.Certificate = *certRsa
		}
	}

	// RSA Public key
	log.Println("Finding RSA public key from cert.")
	rsaPublicKey, ok := kas.Certificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		log.Panic("RSA public key from cert error")
	}
	kas.PublicKeyRsa = *rsaPublicKey

	// EC Cert
	log.Println("Finding EC cert.")
	var ecCert x509.Certificate

	certECHandle, err := findKey(ctx, session, pkcs11.CKO_CERTIFICATE, keyID, ecLabel)
	if err != nil {
		log.Panic(err)
	}
	certECTemplate := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_CERTIFICATE),
		pkcs11.NewAttribute(pkcs11.CKA_CERTIFICATE_TYPE, pkcs11.CKC_X_509),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_VALUE, []byte("")),
		pkcs11.NewAttribute(pkcs11.CKA_SUBJECT, []byte("")),
	}
	ecCertAttrs, err := ctx.GetAttributeValue(session, certECHandle, certECTemplate)
	if err != nil {
		log.Panic(err)
	}
	log.Println(ecCertAttrs)

	for i, a := range ecCertAttrs {
		log.Printf("attr %d, type %d, valuelen %d\n", i, a.Type, len(a.Value))
		if a.Type == pkcs11.CKA_VALUE {
			// exponent := big.NewInt(0)
			// exponent.SetBytes(a.Value)
			certEC, err := x509.ParseCertificate(a.Value)
			if err != nil {
				log.Panic(err)
			}
			ecCert = *certEC
		}
	}

	// EC Public Key
	log.Println("Finding EC public key from cert.")
	log.Println(ecCert.PublicKeyAlgorithm)
	ecPublicKey, ok := ecCert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		log.Panic("EC public key from cert error")
	}
	kas.PublicKeyEc = *ecPublicKey

	// os interrupt
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	// server
	server := http.Server{
		Addr:         "0.0.0.0:" + os.Getenv("SERVER_PORT"),
		ReadTimeout:  timeoutServerRead,
		WriteTimeout: timeoutServerWrite,
		IdleTimeout:  timeoutServerIdle,
	}
	http.HandleFunc("/kas_public_key", kas.CertificateHandler)
	http.HandleFunc("/v2/kas_public_key", kas.PublicKeyHandlerV2)
	http.HandleFunc("/v2/rewrap", kas.Handler)
	// keys
	keySet := jwk.NewSet()
	rsaPublicKeyJwk, err := jwk.FromRaw(kas.PublicKeyRsa)
	if err != nil {
		log.Panic(err)
	}
	err = rsaPublicKeyJwk.Set(jwk.KeyUsageKey, jwk.ForEncryption)
	if err != nil {
		return
	}
	err = keySet.AddKey(rsaPublicKeyJwk)
	if err != nil {
		log.Panic(err)
	}
	err = jwk.AssignKeyID(rsaPublicKeyJwk)
	if err != nil {
		log.Panic(err)
	}
	ecPublicKeyJwk, err := jwk.FromRaw(kas.PublicKeyEc)
	if err != nil {
		log.Panic(err)
	}
	err = ecPublicKeyJwk.Set(jwk.KeyUsageKey, jwk.ForEncryption)
	if err != nil {
		log.Panic(err)
	}
	err = keySet.AddKey(ecPublicKeyJwk)
	if err != nil {
		log.Panic(err)
	}
	err = jwk.AssignKeyID(ecPublicKeyJwk)
	if err != nil {
		log.Panic(err)
	}
	k := keys.Provider{
		Set: keySet,
	}
	http.HandleFunc("/keys", k.Handler)
	// .well-known/opentdf-configuration
	wk := wellknown.Provider{
		OpenTdfConfiguration: wellknown.OpenTdfConfiguration{
			JwksUri: "http://localhost:8080/keys",
			Issuer:  oidcIssuer,
		},
	}
	http.HandleFunc("/.well-known/opentdf-configuration", wk.Handler)
	go func() {
		log.Printf("listening on http://%s", server.Addr)
		if err := server.ListenAndServe(); err != nil {
			log.Println(err)
		}
	}()
	go func() {
		if os.Getenv("SERVER_SECURE_PORT") != "" {
			log.Printf("listening on https://0.0.0.0:%s", os.Getenv("SERVER_SECURE_PORT"))
			if err := http.ListenAndServeTLS(
				"0.0.0.0:"+os.Getenv("SERVER_SECURE_PORT"),
				os.Getenv("SERVER_SECURE_CERTIFICATE_PATH"),
				os.Getenv("SERVER_SECURE_KEY_PATH"),
				nil,
			); err != nil {
				log.Panic(err)
			}
		}
	}()
	<-stop
	err = server.Shutdown(context.Background())
	if err != nil {
		log.Println(err)
	}
}

func findKey(ctx *pkcs11.Ctx, session pkcs11.SessionHandle, class uint, id []byte, label string) (pkcs11.ObjectHandle, error) {
	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, class),
	}
	if len(id) > 0 {
		template = append(template, pkcs11.NewAttribute(pkcs11.CKA_ID, id))
	}
	if label != "" {
		template = append(template, pkcs11.NewAttribute(pkcs11.CKA_LABEL, []byte(label)))
	}

	// CloudHSM does not support CKO_PRIVATE_KEY set to false
	if class == pkcs11.CKO_PRIVATE_KEY {
		template = append(template, pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true))
	}
	var handle pkcs11.ObjectHandle
	var err error
	if err = ctx.FindObjectsInit(session, template); err != nil {
		return handle, errors.Join(ErrHsm, err)
	}
	defer func() {
		finalErr := ctx.FindObjectsFinal(session)
		if err == nil {
			err = finalErr
		}
	}()

	var handles []pkcs11.ObjectHandle
	const maxHandles = 20
	handles, _, err = ctx.FindObjects(session, maxHandles)
	if err != nil {
		return handle, errors.Join(ErrHsm, err)
	}

	switch len(handles) {
	case 0:
		err = fmt.Errorf("key not found")
	case 1:
		handle = handles[0]
	default:
		err = fmt.Errorf("multiple key found")
	}

	return handle, err
}

type Error string

func (e Error) Error() string {
	return string(e)
}
