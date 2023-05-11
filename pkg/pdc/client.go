package pdc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"

	"github.com/grafana/pdc-agent/pkg/httpclient"
	"golang.org/x/crypto/ssh"
)

var (
	// ErrInternal indicates the item could not be processed.
	ErrInternal = errors.New("internal error")
	// ErrInvalidCredentials indicates the auth token is incorrect
	ErrInvalidCredentials = errors.New("invalid credentials")
)

type Config struct {
	Token           string
	HostedGrafanaId string
	URL             *url.URL
}

func (cfg *Config) RegisterFlags(fs *flag.FlagSet) {
	fs.StringVar(&cfg.Token, "token", "", "The token to use to authenticate with Grafana Cloud. It must have the pdc-signing:write scope")
	fs.StringVar(&cfg.HostedGrafanaId, "gcloud-hosted-grafana-id", "", "The ID of the Hosted Grafana instance to connect to")
	fs.Func("api-url", "The URL to the PDC API", cfg.parseApiURL)
}

func (cfg *Config) parseApiURL(s string) error {
	url, err := url.Parse(s)
	if err != nil {
		return err
	}

	cfg.URL = url
	return nil
}

type Client interface {
	SignSSHKey(ctx context.Context, key []byte) (*SigningResponse, error)
}

type SigningResponse struct {
	Certificate ssh.Certificate // use anon struct for unmarshalling, not this one.
	KnownHosts  []byte
}

func (sr *SigningResponse) UnmarshalJSON(data []byte) error {
	target := struct {
		Certificate string `json:"certificate"`
		KnownHosts  string `json:"known_hosts"`
	}{}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err := dec.Decode(&target)
	if err != nil {
		return err
	}

	block, rest := pem.Decode([]byte(target.Certificate))
	if block == nil {
		return fmt.Errorf("failed to pem-decode certificate: %w", err)
	}
	if len(rest) > 0 {
		return fmt.Errorf("only expected one PEM")
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}

	cert, ok := pk.(*ssh.Certificate)
	if !ok {
		return errors.New("public key is not an SSH certificate")
	}

	sr.KnownHosts = []byte(target.KnownHosts)
	sr.Certificate = *cert
	return nil
}

func NewClient(cfg *Config) (Client, error) {
	if cfg.URL == nil {
		return nil, errors.New("api.url cannot be nil")
	}

	return &pdcClient{
		cfg:        cfg,
		httpClient: &http.Client{Transport: httpclient.UserAgentTransport(nil)},
	}, nil
}

type pdcClient struct {
	cfg        *Config
	httpClient *http.Client
}

func (c *pdcClient) SignSSHKey(ctx context.Context, key []byte) (*SigningResponse, error) {
	resp, err := c.call(ctx, http.MethodPost, "/pdc/api/v1/sign-public-key", nil, map[string]string{
		"publicKey": string(key),
	})
	if err != nil {
		return nil, err
	}

	sr := &SigningResponse{}
	err = sr.UnmarshalJSON(resp)
	if err != nil {
		return nil, err
	}

	return sr, nil
}

func (c *pdcClient) call(ctx context.Context, method, rpath string, params map[string]string, body map[string]string) ([]byte, error) {

	url := *c.cfg.URL
	url.Path = path.Join(url.Path, rpath)

	q := url.Query()
	for k, v := range params {
		q.Add(k, v)
	}
	url.RawQuery = q.Encode()

	jsonB, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	// Prepare the request
	req, err := http.NewRequestWithContext(ctx, method, url.String(), bytes.NewBuffer(jsonB))
	if err != nil {
		log.Println("error creating request")
		return nil, ErrInternal
	}

	// base64 id:token for auth
	b := []byte{}
	buf := bytes.NewBuffer(b)
	encoder := base64.NewEncoder(base64.StdEncoding, buf)
	encoder.Write([]byte(c.cfg.HostedGrafanaId + ":" + c.cfg.Token))
	err = encoder.Close()
	if err != nil {
		log.Printf("Failed to encode values for Authorization header: %s\n", err)
		return nil, err
	}

	req.Header.Add("Authorization", "Basic "+buf.String())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("request failed: %s", err)
		return nil, ErrInternal
	}
	defer resp.Body.Close()
	respB, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("failed to read response: %s", err)
		return nil, ErrInternal
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return respB, nil
	case http.StatusUnauthorized:
		return respB, ErrInvalidCredentials
	default:
		log.Println("unknown response from pdc: " + fmt.Sprintf("%d", resp.StatusCode))
		return respB, ErrInternal
	}
}
