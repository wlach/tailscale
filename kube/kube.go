package kube

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path/filepath"
)

const (
	saPath     = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultURL = "https://kubernetes.default.svc"
)

type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

type ObjectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type Secret struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Data       map[string][]byte `json:"data"`
}

type Status struct {
	TypeMeta   `json:",inline"`
	ObjectMeta `json:"metadata"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	Reason     string `json:"reason"`
	Details    struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	Code int `json:"code"`
}

func (s *Status) Error() string {
	return s.Message
}

type Client struct {
	url    string
	ns     string
	client *http.Client
	token  string
}

func readFile(n string) ([]byte, error) {
	return ioutil.ReadFile(filepath.Join(saPath, n))
}

func New() (*Client, error) {
	ns, err := readFile("namespace")
	if err != nil {
		return nil, err
	}
	// TODO: figure out token rotation. it expires frequently.
	token, err := readFile("token")
	if err != nil {
		return nil, err
	}
	caCert, err := readFile("ca.crt")
	if err != nil {
		return nil, err
	}
	cp := x509.NewCertPool()
	if ok := cp.AppendCertsFromPEM(caCert); !ok {
		return nil, fmt.Errorf("error in creating root cert pool")
	}
	return &Client{
		url:   defaultURL,
		ns:    string(ns),
		token: string(token),
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs: cp,
				},
			},
		},
	}, nil
}

func (c *Client) secretURL(name string) string {
	base := fmt.Sprintf("%s/api/v1/namespaces/%s/secrets", c.url, c.ns)
	if name == "" {
		return base
	}
	return base + "/" + name
}

func getError(resp *http.Response) error {
	if resp.StatusCode == 200 {
		return nil
	}
	st := &Status{}
	if err := json.NewDecoder(resp.Body).Decode(st); err != nil {
		return err
	}
	return st
}

func (c *Client) doRequest(method, url string, in, out *Secret) error {
	var body io.Reader
	if in != nil {
		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(in); err != nil {
			return err
		}
		body = &b
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Add("Content-Type", "application/json")
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := getError(resp); err != nil {
		return err
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) GetSecret(name string) (*Secret, error) {
	s := &Secret{Data: make(map[string][]byte)}
	if err := c.doRequest("GET", c.secretURL(name), nil, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (c *Client) CreateSecret(in *Secret) error {
	in.Namespace = c.ns
	return c.doRequest("POST", c.secretURL(""), in, nil)
}

func (c *Client) UpdateSecret(in *Secret) error {
	return c.doRequest("PUT", c.secretURL(in.Name), in, nil)
}
