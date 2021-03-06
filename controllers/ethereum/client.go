package controllers

import (
	"fmt"

	ethereumv1alpha1 "github.com/kotalco/kotal/apis/ethereum/v1alpha1"
)

// EthereumClient is Ethereum client
type EthereumClient interface {
	GetArgs(*ethereumv1alpha1.Node, *ethereumv1alpha1.Network, []string) []string
	GetGenesisFile(*ethereumv1alpha1.Genesis, ethereumv1alpha1.ConsensusAlgorithm) (string, error)
	LoggingArgFromVerbosity(ethereumv1alpha1.VerbosityLevel) string
}

// NewEthereumClient returns an Ethereum client instance
func NewEthereumClient(name ethereumv1alpha1.EthereumClient) (EthereumClient, error) {
	switch name {
	case "besu":
		return &BesuClient{}, nil
	case "geth":
		return &GethClient{}, nil
	default:
		return nil, fmt.Errorf("Client %s is not supported", name)
	}
}
