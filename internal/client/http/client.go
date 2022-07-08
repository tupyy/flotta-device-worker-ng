package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/project-flotta/flotta-operator/models"
	"github.com/tupyy/device-worker-ng/internal/certificate"
	"github.com/tupyy/device-worker-ng/internal/entities"
	"go.uber.org/zap"
)

const (
	certificateKey = "certificate"
	rootUrl        = "/api/flotta-management/v1"
)

// transportWrapper is a wrapper for transport. It can be used as a middleware.
type transportWrapper func(http.RoundTripper) http.RoundTripper

type Client struct {
	// certMananger holds the Certificate Manager
	certMananger *certificate.Manager

	// certificateSignature holds the signature of the client certificate which is used in TLS config.
	// It is used to check if certificates had been updated following registration process.
	certificateSignature []byte

	// server's url
	serverURL *url.URL

	transportWrappers []transportWrapper

	// transport is the transport which make the actual request
	transport http.RoundTripper
}

func New(path string, certManager *certificate.Manager) (*Client, error) {
	if certManager == nil {
		return nil, fmt.Errorf("Certificate manager is missing")
	}

	url, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("Server address error: %s", err)
	}

	// TODO dynamically set based on log level
	transportWrapper := make([]transportWrapper, 0, 1)
	logWrapper := &logTransportWrapper{}
	transportWrapper = append(transportWrapper, logWrapper.Wrap)

	return &Client{
		serverURL:            url,
		certMananger:         certManager,
		certificateSignature: []byte{},
		transportWrappers:    transportWrapper,
	}, nil
}

func (c *Client) Enrol(ctx context.Context, deviceID string, enrolInfo entities.EnrolementInfo) error {
	request, err := newRequestBuilder().
		Type(postDataMessageForDeviceType).
		Action(enrolActionType).
		Header("Content-Type", "application/json").
		Url(fmt.Sprintf("%s/%s/data/%s/out", c.serverURL.String(), rootUrl, deviceID)).
		Body(enrolInfo).
		Build(ctx)

	if err != nil {
		return fmt.Errorf("cannot create enrollment request '%w'", err)
	}

	resp, err := c.do(request)
	if err != nil {
		return fmt.Errorf("cannot enrol device '%w'", err)
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("cannot enrol device. code: %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) Register(ctx context.Context, deviceID string, registerInfo entities.RegistrationInfo) (entities.RegistrationResponse, error) {
	request, err := newRequestBuilder().
		Type(postDataMessageForDeviceType).
		Action(registerActionType).
		Header("Content-Type", "application/json").
		Url(fmt.Sprintf("%s/%s/data/%s/out", c.serverURL.String(), rootUrl, deviceID)).
		Body(registerInfo).
		Build(ctx)

	if err != nil {
		return entities.RegistrationResponse{}, fmt.Errorf("cannot create registration request '%w'", err)
	}

	res, err := c.do(request)
	if err != nil {
		return entities.RegistrationResponse{}, fmt.Errorf("cannot register device '%w'", err)
	}

	message, err := c.processResponse(res)
	if err != nil {
		return entities.RegistrationResponse{}, err
	}

	certMap, ok := message.Content.(map[string]interface{})
	if !ok {
		return entities.RegistrationResponse{}, fmt.Errorf("payload content is not a map")
	}

	cert, ok := certMap[certificateKey]
	if !ok {
		return entities.RegistrationResponse{}, fmt.Errorf("cannot get certificate from payload")
	}

	return entities.RegistrationResponse{SignedCSR: bytes.NewBufferString(cert.(string)).Bytes()}, nil
}

func (c *Client) Heartbeat(ctx context.Context, deviceID string, heartbeat entities.Heartbeat) error {
	request, err := newRequestBuilder().
		Type(postDataMessageForDeviceType).
		Action(heartbeatActionType).
		Url(fmt.Sprintf("%s/%s/data/%s/out", c.serverURL.String(), rootUrl, deviceID)).
		Body(heartbeat).
		Header("Content-Type", "application/json").
		Build(ctx)

	if err != nil {
		return fmt.Errorf("cannot create heartbeat request '%w'", err)
	}

	resp, err := c.do(request)
	if err != nil {
		return fmt.Errorf("cannot send heartbeat '%w'", err)
	}

	// TODO send typed error based on status code
	if resp.StatusCode >= 400 {
		return fmt.Errorf("cannot send heartbeat. code: %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) GetConfiguration(ctx context.Context, deviceID string) (entities.DeviceConfiguration, error) {
	request, err := newRequestBuilder().
		Type(getDataMessageForDeviceType).
		Action(configurationActionType).
		Header("Content-Type", "application/json").
		Url(fmt.Sprintf("%s/%s/data/%s/in", c.serverURL.String(), rootUrl, deviceID)).
		Build(ctx)

	if err != nil {
		return entities.DeviceConfiguration{}, fmt.Errorf("cannot create configuration request '%w'", err)
	}

	res, err := c.do(request)
	if err != nil {
		return entities.DeviceConfiguration{}, fmt.Errorf("cannot get configuration '%w'", err)
	}

	message, err := c.processResponse(res)
	if err != nil {
		return entities.DeviceConfiguration{}, err
	}

	data, ok := message.Content.(map[string]interface{})
	if !ok {
		return entities.DeviceConfiguration{}, fmt.Errorf("payload content is not a map")
	}

	conf, ok := data["configuration"]
	if !ok {
		return entities.DeviceConfiguration{}, fmt.Errorf("cannot find configuration data in payload")
	}

	var m models.DeviceConfiguration

	j, err := json.Marshal(conf)
	if err != nil {
		return entities.DeviceConfiguration{}, fmt.Errorf("cannot read configuration: '%w'", err)
	}

	err = json.Unmarshal(j, &m)
	if err != nil {
		return entities.DeviceConfiguration{}, fmt.Errorf("cannot read configuration: '%w'", err)
	}

	return configurationModel2Entity(m), nil
}

func (c *Client) do(request *http.Request) (*http.Response, error) {
	client, err := c.getClient()
	if err != nil {
		return nil, err
	}

	return client.Do(request)
}

// getClient returns a real http.Client created with our transport.
// It checks if certifcates signatures changed and if true it recreates a new transport.
func (c *Client) getClient() (*http.Client, error) {
	if !bytes.Equal(c.certificateSignature, c.certMananger.Signature()) {
		zap.S().Info("certificates changed. recreate transport")
		t, err := c.createTransport()
		if err != nil {
			return nil, err
		}

		c.certificateSignature = c.certMananger.Signature()

		c.transport = t
	}

	return &http.Client{
		Transport: c.transport,
		Timeout:   2 * time.Second, //TODO to be parametrized
	}, nil

}

func (c *Client) createTransport() (result http.RoundTripper, err error) {
	var tlsConfig *tls.Config

	tlsConfig, err = c.createTLSConfig()

	result = &http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
	}

	// call the other wrappers backwards
	for i := len(c.transportWrappers) - 1; i >= 0; i-- {
		result = c.transportWrappers[i](result)
	}

	return result, err
}

func (c *Client) createTLSConfig() (*tls.Config, error) {
	caRoot, cert, key := c.certMananger.GetCertificates()

	config := tls.Config{
		RootCAs: caRoot,
	}

	certPEM := new(bytes.Buffer)
	err := pem.Encode(certPEM, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})

	privKeyPEM := new(bytes.Buffer)
	switch t := key.(type) {
	case *ecdsa.PrivateKey:
		res, _ := x509.MarshalECPrivateKey(t)
		_ = pem.Encode(privKeyPEM, &pem.Block{
			Type:  "EC PRIVATE KEY",
			Bytes: res,
		})
	case *rsa.PrivateKey:
		_ = pem.Encode(privKeyPEM, &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(t),
		})
	}

	//
	cc, err := tls.X509KeyPair(certPEM.Bytes(), privKeyPEM.Bytes())
	if err != nil {
		return nil, fmt.Errorf("cannot create x509 key pair: %w", err)
	}

	config.Certificates = []tls.Certificate{cc}

	return &config, nil
}

func (c *Client) processResponse(res *http.Response) (models.MessageResponse, error) {
	data, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return models.MessageResponse{}, fmt.Errorf("cannot read response body '%w'", err)
	}
	defer res.Body.Close()

	var message models.MessageResponse
	err = json.Unmarshal(data, &message)
	if err != nil {
		return models.MessageResponse{}, fmt.Errorf("cannot read marshal body into message response '%w'", err)
	}

	return message, nil
}