package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type network struct {
	root                          string
	bitcoin, payer, receiver, lnd []string
	rest, grpc                    string
}

func port(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func command(args []string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %s", args[0], err, string(out)+stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		var value string
		if json.Unmarshal(out, &value) == nil {
			return map[string]any{"value": value}, nil
		}
		return map[string]any{"value": strings.TrimSpace(string(out))}, nil
	}
	return result, nil
}

func run(t *testing.T, base []string, args ...string) map[string]any {
	t.Helper()
	result, err := command(append(append([]string{}, base...), args...))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func await(t *testing.T, label string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: %v", label, err)
}

func startProcess(t *testing.T, root, name string, args ...string) {
	t.Helper()
	path := filepath.Join(root, name+".log")
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(path)
			t.Logf("%s:\n%s", name, data)
		}
	})
}

func startNetwork(t *testing.T) *network {
	t.Helper()
	for _, binary := range []string{"bitcoind", "bitcoin-cli", "lightningd", "lightning-cli", "lnd", "lncli"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("regtest dependency %s: %v", binary, err)
		}
	}
	n := &network{root: t.TempDir()}
	btcDir := filepath.Join(n.root, "bitcoin")
	if err := os.MkdirAll(btcDir, 0700); err != nil {
		t.Fatal(err)
	}
	rpc, p2p, block, tx := port(t), port(t), port(t), port(t)
	n.bitcoin = []string{"bitcoin-cli", "-regtest", "-datadir=" + btcDir, "-rpcport=" + rpc, "-rpcuser=regtest", "-rpcpassword=regtest"}
	startProcess(t, n.root, "bitcoin", "bitcoind", "-regtest", "-datadir="+btcDir, "-rpcport="+rpc, "-port="+p2p, "-rpcuser=regtest", "-rpcpassword=regtest", "-server", "-listen=0", "-dnsseed=0", "-discover=0", "-fallbackfee=0.0002", "-zmqpubrawblock=tcp://127.0.0.1:"+block, "-zmqpubrawtx=tcp://127.0.0.1:"+tx)
	await(t, "Bitcoin RPC", func() error { _, err := command(append(n.bitcoin, "getblockchaininfo")); return err })
	if run(t, n.bitcoin, "getblockchaininfo")["chain"] != "regtest" {
		t.Fatal("Bitcoin is not on regtest")
	}
	run(t, n.bitcoin, "createwallet", "miner")
	address := run(t, n.bitcoin, "getnewaddress")["value"].(string)
	mine := func(blocks string) { run(t, n.bitcoin, "generatetoaddress", blocks, address) }
	mine("101")
	var receiverPort string
	for _, name := range []string{"payer", "receiver"} {
		dir := filepath.Join(n.root, name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		listen := port(t)
		args := []string{"lightningd", "--network=regtest", "--lightning-dir=" + dir, "--addr=127.0.0.1:" + listen, "--bitcoin-rpcconnect=127.0.0.1", "--bitcoin-rpcport=" + rpc, "--bitcoin-rpcuser=regtest", "--bitcoin-rpcpassword=regtest", "--bitcoin-datadir=" + btcDir, "--log-level=debug", "--developer", "--dev-bitcoind-poll=1"}
		cli := []string{"lightning-cli", "--notifications=none", "--json", "--network=regtest", "--lightning-dir=" + dir}
		if name == "payer" {
			n.payer = cli
		} else {
			n.receiver = cli
			receiverPort = listen
			n.rest = "https://127.0.0.1:" + port(t)
			if plugin := os.Getenv("LNURL_CLNREST_PLUGIN"); plugin != "" {
				args = append(args, "--plugin="+plugin)
			}
			args = append(args, "--clnrest-host=127.0.0.1", "--clnrest-port="+strings.TrimPrefix(n.rest, "https://127.0.0.1:"))
		}
		startProcess(t, n.root, name, args...)
		await(t, name+" RPC", func() error { _, err := command(append(cli, "getinfo")); return err })
		if run(t, cli, "getinfo")["network"] != "regtest" {
			t.Fatal(name + " is not on regtest")
		}
	}
	lndDir := filepath.Join(n.root, "lnd")
	n.grpc = "127.0.0.1:" + port(t)
	lndPeer := port(t)
	n.lnd = []string{"lncli", "--network=regtest", "--lnddir=" + lndDir, "--rpcserver=" + n.grpc}
	startProcess(t, n.root, "lnd", "lnd", "--lnddir="+lndDir, "--noseedbackup", "--bitcoin.active", "--bitcoin.regtest", "--bitcoin.node=bitcoind", "--bitcoind.rpchost=127.0.0.1:"+rpc, "--bitcoind.rpcuser=regtest", "--bitcoind.rpcpass=regtest", "--bitcoind.zmqpubrawblock=tcp://127.0.0.1:"+block, "--bitcoind.zmqpubrawtx=tcp://127.0.0.1:"+tx, "--rpclisten="+n.grpc, "--restlisten=127.0.0.1:"+port(t), "--listen=127.0.0.1:"+lndPeer, "--debuglevel=debug")
	await(t, "LND RPC", func() error {
		info, err := command(append(n.lnd, "getinfo"))
		if err == nil && info["synced_to_chain"] != true {
			return fmt.Errorf("LND is not synced")
		}
		return err
	})
	chains, ok := run(t, n.lnd, "getinfo")["chains"].([]any)
	if !ok || len(chains) != 1 || chains[0].(map[string]any)["network"] != "regtest" {
		t.Fatal("LND is not on regtest")
	}
	payerAddress := run(t, n.payer, "newaddr", "p2tr")["p2tr"].(string)
	run(t, n.bitcoin, "sendtoaddress", payerAddress, "2")
	mine("6")
	await(t, "payer funds", func() error {
		funds, err := command(append(n.payer, "listfunds"))
		if err == nil && len(funds["outputs"].([]any)) == 0 {
			return fmt.Errorf("no outputs")
		}
		return err
	})
	clnID := run(t, n.receiver, "getinfo")["id"].(string)
	lndID := run(t, n.lnd, "getinfo")["identity_pubkey"].(string)
	for _, peer := range []struct{ id, port string }{{clnID, receiverPort}, {lndID, lndPeer}} {
		run(t, n.payer, "connect", peer.id, "127.0.0.1", peer.port)
		run(t, n.payer, "-k", "fundchannel", "id="+peer.id, "amount=1000000", "push_msat=500000000", "announce=false")
		mine("6")
		await(t, "channel "+peer.id, func() error {
			channels, err := command(append(n.payer, "listpeerchannels"))
			if err != nil {
				return err
			}
			for _, item := range channels["channels"].([]any) {
				ch := item.(map[string]any)
				if ch["peer_id"] == peer.id && ch["state"] == "CHANNELD_NORMAL" {
					return nil
				}
			}
			return fmt.Errorf("channel not ready")
		})
	}
	return n
}

func (n *network) backendYAML(t *testing.T, name string) string {
	t.Helper()
	switch name {
	case "lnd":
		path := filepath.Join(n.root, "backend.macaroon")
		run(t, n.lnd, "bakemacaroon", "--save_to="+path, "info:read", "invoices:read", "invoices:write", "offchain:read", "offchain:write")
		return fmt.Sprintf("lnd:\n  host: %q\n  tls_cert: %q\n  macaroon: %q\n", n.grpc, filepath.Join(n.root, "lnd", "tls.cert"), path)
	case "cln":
		rune := run(t, n.receiver, "-k", "createrune", `restrictions=[["method=getinfo","method=invoice","method=listinvoices","method=waitanyinvoice","method=pay","method=listpays"]]`)["rune"].(string)
		path := filepath.Join(n.root, "backend.rune")
		if err := os.WriteFile(path, []byte(rune), 0600); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("cln:\n  url: %q\n  rune_file: %q\n  ca_cert: %q\n", n.rest, path, filepath.Join(n.root, "receiver", "regtest", "ca.pem"))
	default:
		t.Fatalf("unknown backend %q", name)
		return ""
	}
}

func (n *network) pay(t *testing.T, bolt11 string) string {
	t.Helper()
	result := run(t, n.payer, "-k", "pay", "bolt11="+bolt11, "retry_for=30")
	preimage, ok := result["payment_preimage"].(string)
	if !ok {
		t.Fatalf("pay returned no preimage: %v", result)
	}
	return preimage
}

func (n *network) invoice(t *testing.T, amountMsat int64) string {
	t.Helper()
	return run(t, n.payer, "invoice", strconv.FormatInt(amountMsat, 10), fmt.Sprintf("withdraw-%d", time.Now().UnixNano()), "withdrawal integration test")["bolt11"].(string)
}
