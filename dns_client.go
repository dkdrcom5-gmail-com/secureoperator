package secureoperator

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/miekg/dns"
)

// ErrInvalidEndpointString is returned when an endpoint string is in an
// unexpected format; the string is expected to be in `ip[:port]` format
var ErrInvalidEndpointString = errors.New("invalid endpoint string")

// ErrFailedParsingIP is returned when the endpoint string looked valid, but
// the IP portion of the string was unable to be parsed
var ErrFailedParsingIP = errors.New("unable to parse IP from string")

// ErrFailedParsingPort is returned when the endpoint string looked valid, but
// the port portion of the string was unable to be parsed
var ErrFailedParsingPort = errors.New("unable to parse port from string")

// ErrAllServersFailed is returned when we failed to reach all configured DNS
// servers
var ErrAllServersFailed = errors.New("unable to reach any of the configured servers")

// exchange is locally set to allow its mocking during testing
var exchange = dns.ExchangeContext

const defaultDNSClientTimeout = 10 * time.Second

// ParseEndpoint parses a string into an Endpoint object, where the endpoint
// string is in the format of "ip:port". If a port is not present in the string,
// the defaultPort is used.
func ParseEndpoint(endpoint string, defaultPort uint16) (ep Endpoint, err error) {
	e := strings.Split(endpoint, ":")

	if len(e) > 2 {
		return ep, ErrInvalidEndpointString
	}

	ip := net.ParseIP(e[0])
	if ip == nil {
		return ep, ErrFailedParsingIP
	}

	ep.IP = ip
	ep.Port = defaultPort

	if len(e) > 1 {
		i, err := strconv.ParseUint(e[1], 10, 16)
		if err != nil {
			return ep, ErrFailedParsingPort
		}

		ep.Port = uint16(i)
	}

	return ep, err
}

// Endpoint represents a host/port combo
type Endpoint struct {
	IP   net.IP
	Port uint16
}

func (e Endpoint) String() string {
	return net.JoinHostPort(e.IP.String(), fmt.Sprintf("%v", e.Port))
}

// Endpoints is a list of Endpoint objects
type Endpoints []Endpoint

// Random retrieves a random Endpoint from a list of Endpoints
func (e Endpoints) Random() Endpoint {
	return e[rand.Intn(len(e))]
}

type dnsCacheRecord struct {
	msg     *dns.Msg
	ips     []net.IP
	expires time.Time
}

func newDNSCache() *dnsCache {
	mutex := sync.Mutex{}

	return &dnsCache{
		mutex:   &mutex,
		records: make(map[string]dnsCacheRecord, 10),
	}
}

type dnsCache struct {
	mutex   *sync.Mutex
	records map[string]dnsCacheRecord
}

func (d *dnsCache) Get(key string) (dnsCacheRecord, bool) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	rec, ok := d.records[key]

	return rec, ok
}

func (d *dnsCache) Set(key string, rec dnsCacheRecord) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	d.records[key] = rec
}

type DNSClientOptions struct {
	Timeout time.Duration
}

// NewSimpleDNSClient creates a SimpleDNSClient
func NewSimpleDNSClient(servers Endpoints, opts *DNSClientOptions) (*SimpleDNSClient, error) {
	if len(servers) < 1 {
		return nil, fmt.Errorf("at least one endpoint server is required")
	}
	if opts == nil {
		opts = &DNSClientOptions{}
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultDNSClientTimeout
	}

	return &SimpleDNSClient{
		servers: servers,
		cache:   newDNSCache(),
		opts:    opts,
	}, nil
}

// SimpleDNSClient is a DNS client, primarily for internal use in secure
// operator.
//
// It provides an in-memory cache, but was optimized to look up one address
// at a time only.
type SimpleDNSClient struct {
	servers Endpoints
	cache   *dnsCache
	opts    *DNSClientOptions
}

// LookupIP does a single lookup against the client's configured DNS servers,
// returning a value from cache if its still valid. It looks at A records only.
func (c *SimpleDNSClient) LookupIP(host string) ([]net.IP, error) {
	// see if cache has the entry; if it's still good, return it
	entry, ok := c.cache.Get(host)
	if ok && entry.expires.After(time.Now()) {
		log.Debugf("simple dns cache hit for %v", host)
		return entry.ips, nil
	}

	// we need to look it up
	for _, server := range c.servers {
		msg := dns.Msg{}
		msg.SetQuestion(dns.Fqdn(host), dns.TypeA)

		ctx, cancel := context.WithTimeout(context.Background(), c.opts.Timeout)
		defer cancel()

		log.Infof("simple dns lookup %v", host)
		r, err := exchange(ctx, &msg, server.String())
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			// was a timeout error; continue to the next server
			continue
		}
		if err != nil {
			return nil, err
		}

		rec := dnsCacheRecord{
			msg: r,
		}

		var shortestTTL uint32

		for _, ans := range r.Answer {
			h := ans.Header()

			if t, ok := ans.(*dns.A); ok {
				rec.ips = append(rec.ips, t.A)

				// if the TTL of this record is the shortest or first seen, use it
				// as the cache record TTL
				if shortestTTL == 0 || h.Ttl < shortestTTL {
					shortestTTL = h.Ttl
				}
			}
		}

		// set the expiry
		rec.expires = time.Now().Add(time.Second * time.Duration(shortestTTL))

		// cache the record
		c.cache.Set(host, rec)

		return rec.ips, nil
	}

	// we didn't reach any server; return a known error
	return nil, ErrAllServersFailed
}
