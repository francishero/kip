/*
Copyright 2019 Elotl Inc.
*/

// Code generated by client-gen. DO NOT EDIT.

package v1beta2

import (
	v1beta2 "github.com/elotl/cloud-instance-provider/pkg/apis/kiyot/v1beta2"
	"github.com/elotl/cloud-instance-provider/pkg/k8sclient/clientset/versioned/scheme"
	rest "k8s.io/client-go/rest"
)

type KiyotV1beta2Interface interface {
	RESTClient() rest.Interface
	CellsGetter
}

// KiyotV1beta2Client is used to interact with features provided by the kiyot.elotl.co group.
type KiyotV1beta2Client struct {
	restClient rest.Interface
}

func (c *KiyotV1beta2Client) Cells() CellInterface {
	return newCells(c)
}

// NewForConfig creates a new KiyotV1beta2Client for the given config.
func NewForConfig(c *rest.Config) (*KiyotV1beta2Client, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}
	return &KiyotV1beta2Client{client}, nil
}

// NewForConfigOrDie creates a new KiyotV1beta2Client for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *rest.Config) *KiyotV1beta2Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}

// New creates a new KiyotV1beta2Client for the given RESTClient.
func New(c rest.Interface) *KiyotV1beta2Client {
	return &KiyotV1beta2Client{c}
}

func setConfigDefaults(config *rest.Config) error {
	gv := v1beta2.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *KiyotV1beta2Client) RESTClient() rest.Interface {
	if c == nil {
		return nil
	}
	return c.restClient
}