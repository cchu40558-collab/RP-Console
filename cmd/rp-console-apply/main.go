// rp-console-apply is a root-only helper. It accepts no command line input and
// can only apply RP Console's fixed Nginx, certificate and UFW locations.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	dataTLSDir = "/var/lib/rp-console/site-tls"
	tlsDir     = "/etc/rp-console/tls"
	nginxSite  = "/etc/nginx/sites-available/rp-console.conf"
)

type request struct {
	Domain string `json:"domain"`
}

type fileSnapshot struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

func main() {
	if os.Geteuid() != 0 {
		fatal(errors.New("must run as root"))
	}
	if err := apply(); err != nil {
		fatal(err)
	}
}

func apply() error {
	var input request
	if err := readJSON(filepath.Join(dataTLSDir, "request.json"), &input); err != nil {
		return fmt.Errorf("read pending site request: %w", err)
	}
	domain, err := validateDomain(input.Domain)
	if err != nil {
		return err
	}
	certPEM, err := readRegular(filepath.Join(dataTLSDir, "origin.crt"))
	if err != nil {
		return fmt.Errorf("read staged certificate: %w", err)
	}
	keyPEM, err := readRegular(filepath.Join(dataTLSDir, "origin.key"))
	if err != nil {
		return fmt.Errorf("read staged key: %w", err)
	}
	if err := validatePair(domain, certPEM, keyPEM); err != nil {
		return err
	}
	if err := enableFirewall(); err != nil {
		return fmt.Errorf("ensure firewall ports: %w", err)
	}
	certSnapshot, err := snapshot(filepath.Join(tlsDir, "origin.crt"))
	if err != nil {
		return err
	}
	keySnapshot, err := snapshot(filepath.Join(tlsDir, "origin.key"))
	if err != nil {
		return err
	}
	nginxSnapshot, err := snapshot(nginxSite)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		clearStaging()
		if committed {
			return
		}
		_ = restore(filepath.Join(tlsDir, "origin.crt"), certSnapshot)
		_ = restore(filepath.Join(tlsDir, "origin.key"), keySnapshot)
		_ = restore(nginxSite, nginxSnapshot)
		_ = run("nginx", "-t")
		_ = reloadNginx()
	}()
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(tlsDir, "origin.crt"), certPEM, 0644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(tlsDir, "origin.key"), keyPEM, 0600); err != nil {
		return err
	}
	if err := replaceNginxSite(domain); err != nil {
		return err
	}
	committed = true
	return nil
}

func snapshot(filename string) (fileSnapshot, error) {
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{}, nil
	}
	if err != nil {
		return fileSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return fileSnapshot{}, errors.New("existing file is not regular")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return fileSnapshot{}, err
	}
	return fileSnapshot{exists: true, data: data, mode: info.Mode().Perm()}, nil
}

func restore(filename string, original fileSnapshot) error {
	if !original.exists {
		return os.Remove(filename)
	}
	return writeFile(filename, original.data, original.mode)
}

func clearStaging() {
	_ = os.Remove(filepath.Join(dataTLSDir, "request.json"))
	_ = os.Remove(filepath.Join(dataTLSDir, "origin.crt"))
	_ = os.Remove(filepath.Join(dataTLSDir, "origin.key"))
}

func readJSON(filename string, target any) error {
	raw, err := readRegular(filename)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func readRegular(filename string) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 768<<10 {
		return nil, errors.New("must be a small regular file")
	}
	return os.ReadFile(filename)
}

func validateDomain(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(value))
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return "", errors.New("invalid domain")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid domain")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return "", errors.New("invalid domain")
			}
		}
	}
	return domain, nil
}

func validatePair(domain string, certPEM, keyPEM []byte) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) == 0 {
		return errors.New("invalid or mismatched TLS certificate and private key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("invalid TLS certificate")
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return errors.New("certificate does not contain the configured domain")
	}
	if time.Now().After(leaf.NotAfter) {
		return errors.New("TLS certificate is expired")
	}
	return nil
}

func writeFile(filename string, content []byte, mode os.FileMode) error {
	temporary := filename + ".new"
	if err := os.WriteFile(temporary, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(temporary, mode); err != nil {
		return err
	}
	return os.Rename(temporary, filename)
}

func replaceNginxSite(domain string) error {
	previous, readErr := os.ReadFile(nginxSite)
	existed := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := writeFile(nginxSite, []byte(nginxConfig(domain)), 0644); err != nil {
		return err
	}
	if err := run("nginx", "-t"); err == nil {
		if err = reloadNginx(); err == nil {
			return nil
		}
	}
	if existed {
		_ = writeFile(nginxSite, previous, 0644)
	} else {
		_ = os.Remove(nginxSite)
	}
	_ = run("nginx", "-t")
	_ = reloadNginx()
	return errors.New("nginx rejected the updated RP Console site; the previous configuration was restored")
}

func nginxConfig(domain string) string {
	return fmt.Sprintf(`server {
    listen 80;
    listen [::]:80;
    server_name %s;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name %s;
    ssl_certificate /etc/rp-console/tls/origin.crt;
    ssl_certificate_key /etc/rp-console/tls/origin.key;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_timeout 1d;
    location / {
        proxy_pass http://127.0.0.1:2053;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
`, domain, domain)
}

func enableFirewall() error {
	if _, err := exec.LookPath("ufw"); err != nil {
		return nil
	}
	status, err := runOutput(20*time.Second, "ufw", "status")
	if err != nil {
		return err
	}
	if strings.Contains(string(status), "Status: inactive") {
		return nil
	}
	for _, port := range []string{"80", "443"} {
		if firewallPortAllowed(status, port) {
			continue
		}
		if _, err := runOutput(120*time.Second, "ufw", "allow", port+"/tcp"); err != nil {
			return fmt.Errorf("allow %s/tcp: %w", port, err)
		}
	}
	return nil
}

func firewallPortAllowed(status []byte, port string) bool {
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && fields[0] == port+"/tcp" && fields[1] == "ALLOW" && fields[2] == "IN" && fields[3] == "Anywhere" {
			return true
		}
	}
	return false
}

func run(name string, args ...string) error {
	_, err := runOutput(30*time.Second, name, args...)
	return err
}

func runOutput(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := command.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return output, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	return output, err
}

func reloadNginx() error {
	if err := run("systemctl", "reload", "nginx"); err == nil {
		return nil
	}
	return run("service", "nginx", "reload")
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "rp-console-apply:", err); os.Exit(1) }
