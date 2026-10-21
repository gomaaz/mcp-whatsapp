// Command whatsapp-mcp is a single-binary WhatsApp bridge that runs as an
// HTTP MCP daemon (default 127.0.0.1:8765). Subcommands:
//
//	login  — one-shot interactive QR pairing (writes session to store/whatsapp.db)
//	serve  — long-running HTTP daemon: serves MCP at /mcp and pairing UI at /pair
//	smoke  — non-interactive boot-check used by CI (no network, no listener)
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	mcpsrv "github.com/sealjay/mcp-whatsapp/internal/mcp"
	"github.com/sealjay/mcp-whatsapp/internal/security"
)

// Version is overridden at build time via -ldflags "-X main.Version=...".
var Version = "dev"

const usage = `whatsapp-mcp: WhatsApp bridge over MCP

Usage:
  whatsapp-mcp <command> [flags]
  whatsapp-mcp -version

Commands:
  login   Pair this device with your WhatsApp account via QR code (terminal)
  serve   Run the always-on HTTP MCP daemon (tracks events + serves MCP on 127.0.0.1:8765)
  smoke   Non-interactive boot check (CI): opens store + builds MCP server without connecting
  help    Show this help

Global flags (before the command):
  -store DIR   Directory holding messages.db and whatsapp.db (default: ./store)
  -debug       Show full JIDs, message IDs, bodies and names in logs (default: redacted)
  -read-only   Register only tools without outbound effect: no sending, editing,
               deleting, read receipts or group/privacy changes
               (env WHATSAPP_MCP_READ_ONLY=1; the flag wins when both are set)
  -keep-revoked
               Keep the text of messages their sender deleted for everyone;
               by default it is blanked and the message shows as deleted
               (env WHATSAPP_MCP_KEEP_REVOKED=1; the flag wins when both are set)
  -version     Print version and exit

Serve-specific flags (after 'serve'):
  -addr host:port   HTTP bind address (default: 127.0.0.1:8765, env WHATSAPP_MCP_ADDR)
  -allow-remote     Allow binding to a non-loopback address (dangerous)
`

func main() {
	mcpsrv.Version = strings.TrimPrefix(Version, "v")

	// Handle -version / --version before subcommand dispatch so it works
	// regardless of the command that follows (or if none is given).
	for _, a := range os.Args[1:] {
		if a == "-version" || a == "--version" {
			fmt.Printf("whatsapp-mcp %s\n", Version)
			os.Exit(0)
		}
		// Stop scanning at the first non-flag token: that's the subcommand,
		// and a later `-version` belongs to the subcommand's FlagSet.
		if len(a) == 0 || a[0] != '-' {
			break
		}
	}

	var (
		storeDir    string
		debug       bool
		readOnly    bool
		keepRevoked bool
	)
	fs := flag.NewFlagSet("whatsapp-mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&storeDir, "store", "./store", "directory holding messages.db and whatsapp.db")
	fs.BoolVar(&debug, "debug", false, "show full JIDs, message IDs, bodies and names in logs (default: redacted)")
	fs.BoolVar(&readOnly, "read-only", false, "register only tools without outbound effect (env WHATSAPP_MCP_READ_ONLY=1)")
	fs.BoolVar(&keepRevoked, "keep-revoked", false, "keep the text of messages deleted by their sender (env WHATSAPP_MCP_KEEP_REVOKED=1)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	readOnly = flagOrEnv(set["read-only"], readOnly, os.Getenv("WHATSAPP_MCP_READ_ONLY"))
	keepRevoked = flagOrEnv(set["keep-revoked"], keepRevoked, os.Getenv("WHATSAPP_MCP_KEEP_REVOKED"))
	args := fs.Args()
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if os.Getenv("WHATSAPP_MCP_DEBUG") == "1" {
		debug = true
	}
	redactor := &security.Redactor{Debug: debug}

	cmd, rest := args[0], args[1:]
	var code int
	switch cmd {
	case "login":
		code = runLogin(storeDir, redactor, rest)
	case "serve":
		code = runServe(storeDir, redactor, readOnly, keepRevoked, rest)
	case "smoke":
		code = runSmoke(storeDir, redactor, rest)
	case "help", "-h", "--help":
		fmt.Fprint(os.Stdout, usage)
		code = 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

// flagOrEnv applies the precedence shared by the boolean global switches
// (-read-only / WHATSAPP_MCP_READ_ONLY, -keep-revoked /
// WHATSAPP_MCP_KEEP_REVOKED): an explicitly passed flag (true or false) wins,
// otherwise the env var turns the switch on when set to "1".
func flagOrEnv(flagSet, flagValue bool, env string) bool {
	if flagSet {
		return flagValue
	}
	return env == "1"
}
