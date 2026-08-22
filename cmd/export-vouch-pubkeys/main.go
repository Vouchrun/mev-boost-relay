// Command export-vouch-pubkeys enumerates the Vouch validator registry from the
// NodeDeposit contract and writes a static pubkeys file for the relay's
// -vouch-pubkeys-file fallback (see the relay-deploy runbook, "Registry
// warm-start"). Enumeration uses the per-index pubkeysOfNode getter across all
// nodes — never getPubkeysOfNode (quadratic memory wall).
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/flashbots/mev-boost-relay/services/registry"
	"github.com/spf13/cobra"
)

const defaultRegistryAddress = "0x3f82615aE0C027d587FD0d04d9EaCc8f0BaCFf94"

var (
	registryAddress string
	rpcURL          string
	outFile         string
)

func main() {
	if err := exportCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var exportCmd = &cobra.Command{
	Use:   "export-vouch-pubkeys",
	Short: "export the Vouch validator pubkey registry to a static file",
	Run: func(cmd *cobra.Command, args []string) {
		if rpcURL == "" {
			fmt.Fprintln(os.Stderr, "error: -rpc is required (EL JSON-RPC URL)")
			os.Exit(1)
		}
		if outFile == "" {
			fmt.Fprintln(os.Stderr, "error: -out is required (output file path)")
			os.Exit(1)
		}
		contractAddr, err := registry.ParseAddress(registryAddress)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid -registry-address: %v\n", err)
			os.Exit(1)
		}

		start := time.Now()
		pubkeys, nodes, err := registry.EnumeratePubkeys(rpcURL, common.Address(contractAddr))
		if err != nil {
			// Operator must know the file would be incomplete — fail, do not write.
			fmt.Fprintf(os.Stderr, "error: registry enumeration failed (no file written): %v\n", err)
			os.Exit(1)
		}
		if err := registry.WritePubkeysFile(outFile, pubkeys); err != nil {
			fmt.Fprintf(os.Stderr, "error: could not write %s: %v\n", outFile, err)
			os.Exit(1)
		}

		fmt.Printf("vouch pubkeys exported: %d nodes enumerated, %d pubkeys written to %s, elapsed %s\n",
			nodes, len(pubkeys), outFile, time.Since(start).Round(time.Millisecond))
	},
}

func init() {
	exportCmd.Flags().StringVar(&registryAddress, "registry-address", defaultRegistryAddress,
		"NodeDeposit contract address for the Vouch validator registry")
	exportCmd.Flags().StringVar(&rpcURL, "rpc", "", "EL JSON-RPC URL (required)")
	exportCmd.Flags().StringVar(&outFile, "out", "", "output file path (required)")
}
