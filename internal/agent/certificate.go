package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type CertificateResult struct {
	Domain          string     `json:"domain"`
	Issuer          string     `json:"issuer,omitempty"`
	Subject         string     `json:"subject,omitempty"`
	SerialNumber    string     `json:"serialNumber,omitempty"`
	DNSNames        []string   `json:"dnsNames,omitempty"`
	ValidFrom       *time.Time `json:"validFrom,omitempty"`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	CheckedAt       time.Time  `json:"checkedAt"`
	HostnameValid   bool       `json:"hostnameValid"`
	ResolvedAddress string     `json:"resolvedAddress,omitempty"`
	Error           string     `json:"error,omitempty"`
}

func runCertificate(parent context.Context, cfg Config, request TaskRequest, timeout time.Duration) (TaskResponse, error) {
	host, port, err := normalizeCertificateTarget(request.Target, request.Options.Ports)
	if err != nil {
		return TaskResponse{}, err
	}
	request.Target = strings.TrimSpace(request.Target)

	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	certificate := inspectCertificate(ctx, host, port)
	finished := time.Now().UTC()
	hostname, _ := osHostname()

	return TaskResponse{
		Version:    "v1",
		TaskID:     request.TaskID,
		Type:       request.Type,
		Target:     request.Target,
		Agent:      AgentInfo{Name: cfg.AgentName, Hostname: hostname},
		Status:     "completed",
		StartedAt:  started,
		FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(),
		Result: ProbeResult{
			NormalizedTarget: host,
			Available:        certificate.Error == "",
			Certificate:      &certificate,
		},
	}, nil
}

func normalizeCertificateTarget(raw string, requestedPorts []int) (string, int, error) {
	ports := requestedPorts
	if len(ports) == 0 {
		ports = []int{443}
	}
	if len(ports) != 1 {
		return "", 0, fmt.Errorf("certificate tasks require exactly one port")
	}
	spec, err := normalizeTarget(raw, ports)
	if err != nil {
		return "", 0, err
	}
	return spec.host, ports[0], nil
}

func inspectCertificate(ctx context.Context, domain string, port int) CertificateResult {
	checkedAt := time.Now().UTC()
	result := CertificateResult{Domain: domain, CheckedAt: checkedAt}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		Config: &tls.Config{
			ServerName:         domain,
			InsecureSkipVerify: true, //nolint:gosec -- certificate validity is reported separately.
		},
	}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(domain, strconv.Itoa(port)))
	if err != nil {
		result.Error = fmt.Sprintf("TLS connection failed: %v", err)
		return result
	}
	defer connection.Close()
	result.ResolvedAddress = connection.RemoteAddr().String()

	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		result.Error = "unexpected TLS connection type"
		return result
	}
	state := tlsConnection.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		result.Error = "server returned no certificate"
		return result
	}

	leaf := state.PeerCertificates[0]
	validFrom := leaf.NotBefore.UTC()
	expiresAt := leaf.NotAfter.UTC()
	result.Issuer = leaf.Issuer.CommonName
	if result.Issuer == "" {
		result.Issuer = leaf.Issuer.String()
	}
	result.Subject = leaf.Subject.CommonName
	if result.Subject == "" {
		result.Subject = leaf.Subject.String()
	}
	result.SerialNumber = leaf.SerialNumber.Text(16)
	result.DNSNames = append([]string(nil), leaf.DNSNames...)
	result.ValidFrom = &validFrom
	result.ExpiresAt = &expiresAt
	result.HostnameValid = leaf.VerifyHostname(domain) == nil
	return result
}
