package gobusterdns

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/OJ/gobuster/v3/libgobuster"
	"github.com/google/uuid"
)

// ErrWildcard is returned if a wildcard response is found
type ErrWildcard struct {
	wildcardIps libgobuster.Set[netip.Addr]
}

// Error is the implementation of the error interface
func (e *ErrWildcard) Error() string {
	return fmt.Sprintf("the DNS Server returned the same IP for every domain. IP address(es) returned: %s", e.wildcardIps.Stringify())
}

// GobusterDNS is the main type to implement the interface
type GobusterDNS struct {
	resolver    *net.Resolver
	globalopts  *libgobuster.Options
	options     *OptionsDNS
	isWildcard  bool
	wildcardIps libgobuster.Set[netip.Addr]
}

func newCustomDialer(server string) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		d := net.Dialer{}
		if !strings.Contains(server, ":") {
			server = fmt.Sprintf("%s:53", server)
		}
		return d.DialContext(ctx, "udp", server)
	}
}

// NewGobusterDNS creates a new initialized GobusterDNS
func NewGobusterDNS(globalopts *libgobuster.Options, opts *OptionsDNS) (*GobusterDNS, error) {
	if globalopts == nil {
		return nil, fmt.Errorf("please provide valid global options")
	}

	if opts == nil {
		return nil, fmt.Errorf("please provide valid plugin options")
	}

	resolver := net.DefaultResolver
	if opts.Resolver != "" {
		resolver = &net.Resolver{
			PreferGo: true,
			Dial:     newCustomDialer(opts.Resolver),
		}
	}

	g := GobusterDNS{
		options:     opts,
		globalopts:  globalopts,
		wildcardIps: libgobuster.NewSet[netip.Addr](),
		resolver:    resolver,
	}
	return &g, nil
}

// Name should return the name of the plugin
func (d *GobusterDNS) Name() string {
	return "DNS enumeration"
}

// PreRun is the pre run implementation of gobusterdns
func (d *GobusterDNS) PreRun(ctx context.Context, progress *libgobuster.Progress) error {
	// Resolve a subdomain that probably shouldn't exist
	guid := uuid.New()
	wildcardIps, err := d.dnsLookup(ctx, fmt.Sprintf("%s.%s", guid, d.options.Domain))
	if err == nil {
		d.isWildcard = true
		d.wildcardIps.AddRange(wildcardIps)
		if !d.options.WildcardForced {
			return &ErrWildcard{wildcardIps: d.wildcardIps}
		}
	}

	if !d.globalopts.Quiet {
		// Provide a warning if the base domain doesn't resolve (in case of typo)
		_, err = d.dnsLookup(ctx, d.options.Domain)
		if err != nil {
			// Not an error, just a warning. Eg. `yp.to` doesn't resolve, but `cr.yp.to` does!
			progress.MessageChan <- libgobuster.Message{
				Level:   libgobuster.LevelInfo,
				Message: fmt.Sprintf("[-] Unable to validate base domain: %s (%v)", d.options.Domain, err),
			}
			progress.MessageChan <- libgobuster.Message{
				Level:   libgobuster.LevelDebug,
				Message: fmt.Sprintf("%#v", err),
			}
		}
	}

	return nil
}

// ProcessWord is the process implementation of gobusterdns
func (d *GobusterDNS) ProcessWord(ctx context.Context, word string, progress *libgobuster.Progress) error {
	subdomain := fmt.Sprintf("%s.%s", word, d.options.Domain)
	if !d.options.NoFQDN && !strings.HasSuffix(subdomain, ".") {
		// add a . to indicate this is the full domain and we do not want to traverse the search domains on the system
		subdomain = fmt.Sprintf("%s.", subdomain)
	}

    // Check if the -e (ResolveToIP) option is set
    if d.options.ResolveToIP != "" {
		// Use ResolveToIP if -e is specified
		client := &http.Client{Timeout: d.options.Timeout}
		url := fmt.Sprintf("http://%s", d.options.ResolveToIP)
		req, _ := http.NewRequest("GET", url, nil)
		req.Host = subdomain // Set Host header for subdomain
	
		// Execute the HTTP request and check if subdomain is reachable
		resp, err := client.Do(req)
		if err != nil {
			return nil // Skip if unreachable
		}
		defer resp.Body.Close()
	
		// Check if response meets reachability criteria (e.g., content length > 100)
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		if buf.Len() > 100 {
			result := Result{
				Subdomain: subdomain,
				Found:     true,
				ShowIPs:   d.options.ShowIPs,
				ShowCNAME: d.options.ShowCNAME,
				NoFQDN:    d.options.NoFQDN,
			}
			if d.options.ShowIPs {
				result.IPs = []netip.Addr{netip.MustParseAddr(d.options.ResolveToIP)}
			}
			progress.ResultChan <- result
		}
		return nil
	}

    // use dnsLookup if -e is not specified
	ips, err := d.dnsLookup(ctx, subdomain)
	if err == nil {
		if !d.isWildcard || !d.wildcardIps.ContainsAny(ips) {
			result := Result{
				Subdomain: subdomain,
				Found:     true,
				ShowIPs:   d.options.ShowIPs,
				ShowCNAME: d.options.ShowCNAME,
				NoFQDN:    d.options.NoFQDN,
			}
			if d.options.ShowIPs {
				result.IPs = ips
			} else if d.options.ShowCNAME {
				cname, err := d.dnsLookupCname(ctx, subdomain)
				if err == nil {
					result.CNAME = cname
				}
			}
			progress.ResultChan <- result
		}
	} else if d.globalopts.Verbose {
		progress.ResultChan <- Result{
			Subdomain: subdomain,
			Found:     false,
			ShowIPs:   d.options.ShowIPs,
			ShowCNAME: d.options.ShowCNAME,
		}
	}
	return nil
}

func (d *GobusterDNS) AdditionalWords(word string) []string {
	return []string{}
}

// GetConfigString returns the string representation of the current config
func (d *GobusterDNS) GetConfigString() (string, error) {
	var buffer bytes.Buffer
	bw := bufio.NewWriter(&buffer)
	tw := tabwriter.NewWriter(bw, 0, 5, 3, ' ', 0)
	o := d.options

	if _, err := fmt.Fprintf(tw, "[+] Domain:\t%s\n", o.Domain); err != nil {
		return "", err
	}

	if _, err := fmt.Fprintf(tw, "[+] Threads:\t%d\n", d.globalopts.Threads); err != nil {
		return "", err
	}

	if d.globalopts.Delay > 0 {
		if _, err := fmt.Fprintf(tw, "[+] Delay:\t%s\n", d.globalopts.Delay); err != nil {
			return "", err
		}
	}

	if o.Resolver != "" {
		if _, err := fmt.Fprintf(tw, "[+] Resolver:\t%s\n", o.Resolver); err != nil {
			return "", err
		}
	}

	if o.ShowCNAME {
		if _, err := fmt.Fprintf(tw, "[+] Show CNAME:\ttrue\n"); err != nil {
			return "", err
		}
	}

	if o.ShowIPs {
		if _, err := fmt.Fprintf(tw, "[+] Show IPs:\ttrue\n"); err != nil {
			return "", err
		}
	}

	if o.WildcardForced {
		if _, err := fmt.Fprintf(tw, "[+] Wildcard forced:\ttrue\n"); err != nil {
			return "", err
		}
	}

	if _, err := fmt.Fprintf(tw, "[+] Timeout:\t%s\n", o.Timeout.String()); err != nil {
		return "", err
	}

	wordlist := "stdin (pipe)"
	if d.globalopts.Wordlist != "-" {
		wordlist = d.globalopts.Wordlist
	}
	if _, err := fmt.Fprintf(tw, "[+] Wordlist:\t%s\n", wordlist); err != nil {
		return "", err
	}

	if d.globalopts.PatternFile != "" {
		if _, err := fmt.Fprintf(tw, "[+] Patterns:\t%s (%d entries)\n", d.globalopts.PatternFile, len(d.globalopts.Patterns)); err != nil {
			return "", err
		}
	}

	if d.globalopts.Verbose {
		if _, err := fmt.Fprintf(tw, "[+] Verbose:\ttrue\n"); err != nil {
			return "", err
		}
	}

	if err := tw.Flush(); err != nil {
		return "", fmt.Errorf("error on tostring: %w", err)
	}

	if err := bw.Flush(); err != nil {
		return "", fmt.Errorf("error on tostring: %w", err)
	}

	return strings.TrimSpace(buffer.String()), nil
}

func (d *GobusterDNS) dnsLookup(ctx context.Context, domain string) ([]netip.Addr, error) {
	ctx2, cancel := context.WithTimeout(ctx, d.options.Timeout)
	defer cancel()
	return d.resolver.LookupNetIP(ctx2, "ip", domain)
}

func (d *GobusterDNS) dnsLookupCname(ctx context.Context, domain string) (string, error) {
	ctx2, cancel := context.WithTimeout(ctx, d.options.Timeout)
	defer cancel()
	time.Sleep(time.Second)
	return d.resolver.LookupCNAME(ctx2, domain)
}
