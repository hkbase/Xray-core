package tls

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	utls "github.com/refraction-networking/utls"
	"github.com/xtls/xray-core/common/crypto"
	dns2 "github.com/xtls/xray-core/features/dns"
	"golang.org/x/net/http2"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/xtls/reality"
	"github.com/xtls/reality/hpke"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/utils"
	"github.com/xtls/xray-core/transport/internet"
	"golang.org/x/crypto/cryptobyte"
)

func ApplyECH(c *Config, config *tls.Config) error {
	var ECHConfig []byte
	var err error

	var nameToQuery string
	if net.ParseAddress(config.ServerName).Family().IsDomain() {
		nameToQuery = config.ServerName
	}
	var DNSServer string

	// for server
	if len(c.EchServerKeys) != 0 {
		KeySets, err := ConvertToGoECHKeys(c.EchServerKeys)
		if err != nil {
			return errors.New("Failed to unmarshal ECHKeySetList: ", err)
		}
		config.EncryptedClientHelloKeys = KeySets
	}

	// for client
	if len(c.EchConfigList) != 0 {
		defer func() {
			// if failed to get ECHConfig, use an invalid one to make connection fail
			if err != nil {
				if c.EchForceQuery {
					ECHConfig = []byte{1, 1, 4, 5, 1, 4}
				}
			}
			config.EncryptedClientHelloConfigList = ECHConfig
		}()
		// direct base64 config
		if strings.Contains(c.EchConfigList, "://") {
			// query config from dns
			parts := strings.Split(c.EchConfigList, "+")
			if len(parts) == 2 {
				// parse ECH DNS server in format of "example.com+https://1.1.1.1/dns-query"
				nameToQuery = parts[0]
				DNSServer = parts[1]
			} else if len(parts) == 1 {
				// normal format
				DNSServer = parts[0]
			} else {
				return errors.New("Invalid ECH DNS server format: ", c.EchConfigList)
			}
			if nameToQuery == "" {
				return errors.New("Using DNS for ECH Config needs serverName or use Server format example.com+https://1.1.1.1/dns-query")
			}
			ECHConfig, err = QueryRecord(nameToQuery, DNSServer, c.EchForceQuery, c.EchSocketSettings)
			if err != nil {
				return errors.New("Failed to query ECH DNS record for domain: ", nameToQuery, " at server: ", DNSServer).Base(err)
			}
		} else {
			ECHConfig, err = base64.StdEncoding.DecodeString(c.EchConfigList)
			if err != nil {
				return errors.New("Failed to unmarshal ECHConfigList: ", err)
			}
		}
	}

	return nil
}

type ECHConfigCache struct {
	configRecord atomic.Pointer[echConfigRecord]
	// updateLock is not for preventing concurrent read/write, but for preventing concurrent update
	UpdateLock sync.Mutex
}

type echConfigRecord struct {
	config []byte
	expire time.Time
	err    error
}

var (
	// key value must be like this: "example.com|udp://1.1.1.1"
	GlobalECHConfigCache = utils.NewTypedSyncMap[string, *ECHConfigCache]()
	clientForECHDOH      = utils.NewTypedSyncMap[string, *http.Client]()
)

// Update updates the ECH config for given domain and server.
// this method is concurrent safe, only one update request will be sent, others get the cache.
// if isLockedUpdate is true, it will not try to acquire the lock.
func (c *ECHConfigCache) Update(domain string, server string, isLockedUpdate bool, forceQuery bool, sockopt *internet.SocketConfig) ([]byte, error) {
	if !isLockedUpdate {
		c.UpdateLock.Lock()
		defer c.UpdateLock.Unlock()
	}
	// Double check cache after acquiring lock
	configRecord := c.configRecord.Load()
	if configRecord.expire.After(time.Now()) {
		errors.LogDebug(context.Background(), "Cache hit for domain after double check: ", domain)
		return configRecord.config, configRecord.err
	}
	// Query ECH config from DNS server
	errors.LogDebug(context.Background(), "Trying to query ECH config for domain: ", domain, " with ECH server: ", server)
	echConfig, ttl, err := dnsQuery(server, domain, sockopt)
	if err != nil {
		if forceQuery || ttl == 0 {
			return nil, err
		}
	}
	configRecord = &echConfigRecord{
		config: echConfig,
		expire: time.Now().Add(time.Duration(ttl) * time.Second),
		err:    err,
	}
	c.configRecord.Store(configRecord)
	return configRecord.config, configRecord.err
}

// QueryRecord returns the ECH config for given domain.
// If the record is not in cache or expired, it will query the DNS server and update the cache.
func QueryRecord(domain string, server string, forceQuery bool, sockopt *internet.SocketConfig) ([]byte, error) {
	GlobalECHConfigCacheKey := domain + "|" + server + "|" + fmt.Sprintf("%p", sockopt)
	echConfigCache, ok := GlobalECHConfigCache.Load(GlobalECHConfigCacheKey)
	if !ok {
		echConfigCache = &ECHConfigCache{}
		echConfigCache.configRecord.Store(&echConfigRecord{})
		echConfigCache, _ = GlobalECHConfigCache.LoadOrStore(GlobalECHConfigCacheKey, echConfigCache)
	}
	configRecord := echConfigCache.configRecord.Load()
	if configRecord.expire.After(time.Now()) {
		errors.LogDebug(context.Background(), "Cache hit for domain: ", domain)
		return configRecord.config, configRecord.err
	}

	// If expire is zero value, it means we are in initial state, wait for the query to finish
	// otherwise return old value immediately and update in a goroutine
	// but if the cache is too old, wait for update
	if configRecord.expire == (time.Time{}) || configRecord.expire.Add(time.Hour*6).Before(time.Now()) {
		return echConfigCache.Update(domain, server, false, forceQuery, sockopt)
	} else {
		// If someone already acquired the lock, it means it is updating, do not start another update goroutine
		if echConfigCache.UpdateLock.TryLock() {
			go func() {
				defer echConfigCache.UpdateLock.Unlock()
				echConfigCache.Update(domain, server, true, forceQuery, sockopt)
			}()
		}
		return configRecord.config, configRecord.err
	}
}

// dnsQuery is the real func for sending type65 query for given domain to given DNS server.
// return ECH config, TTL and error
func dnsQuery(server string, domain string, sockopt *internet.SocketConfig) ([]byte, uint32, error) {
	m := new(dns.Msg)
	var dnsResolve []byte
	m.SetQuestion(dns.Fqdn(domain), dns.TypeHTTPS)
	// for DOH server
	if strings.HasPrefix(server, "https://") || strings.HasPrefix(server, "h2c://") {
		h2c := strings.HasPrefix(server, "h2c://")
		m.SetEdns0(4096, false) // 4096 is the buffer size, false means no DNSSEC
		padding := &dns.EDNS0_PADDING{Padding: make([]byte, int(crypto.RandBetween(100, 300)))}
		if opt := m.IsEdns0(); opt != nil {
			opt.Option = append(opt.Option, padding)
		}
		// always 0 in DOH
		m.Id = 0
		msg, err := m.Pack()
		if err != nil {
			return nil, 0, err
		}
		var client *http.Client
		serverKey := server + "|" + fmt.Sprintf("%p", sockopt)
		if client, _ = clientForECHDOH.Load(serverKey); client == nil {
			// All traffic sent by core should via xray's internet.DialSystem
			// This involves the behavior of some Android VPN GUI clients
			tr := &http2.Transport{
				IdleConnTimeout: net.ConnIdleTimeout,
				ReadIdleTimeout: net.ChromeH2KeepAlivePeriod,
				DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
					dest, err := net.ParseDestination(network + ":" + addr)
					if err != nil {
						return nil, err
					}
					var conn net.Conn

					conn, err = internet.DialSystem(ctx, dest, sockopt)
					if err != nil {
						return nil, err
					}

					if !h2c {
						u, err := url.Parse(server)
						if err != nil {
							return nil, err
						}
						conn = utls.UClient(conn, &utls.Config{ServerName: u.Hostname()}, utls.HelloChrome_Auto)
						if err := conn.(*utls.UConn).HandshakeContext(ctx); err != nil {
							return nil, err
						}
					}
					return conn, nil
				},
			}
			c := &http.Client{
				Timeout:   5 * time.Second,
				Transport: tr,
			}
			client, _ = clientForECHDOH.LoadOrStore(serverKey, c)
		}
		req, err := http.NewRequest("POST", server, bytes.NewReader(msg))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Accept", "application/dns-message")
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("X-Padding", strings.Repeat("X", int(crypto.RandBetween(100, 1000))))
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, 0, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, 0, errors.New("query failed with response code:", resp.StatusCode)
		}
		dnsResolve = respBody
	} else if strings.HasPrefix(server, "udp://") { // for classic udp dns server
		udpServerAddr := server[len("udp://"):]
		// default port 53 if not specified
		if !strings.Contains(udpServerAddr, ":") {
			udpServerAddr = udpServerAddr + ":53"
		}
		dest, err := net.ParseDestination("udp" + ":" + udpServerAddr)
		if err != nil {
			return nil, 0, errors.New("failed to parse udp dns server ", udpServerAddr, " for ECH: ", err)
		}
		dnsTimeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// use xray's internet.DialSystem as mentioned above
		conn, err := internet.DialSystem(dnsTimeoutCtx, dest, sockopt)
		if err != nil {
			return nil, 0, err
		}
		defer func() {
			err := conn.Close()
			if err != nil {
				errors.LogDebug(context.Background(), "Failed to close connection: ", err)
			}
		}()
		msg, err := m.Pack()
		if err != nil {
			return nil, 0, err
		}
		conn.Write(msg)
		udpResponse := make([]byte, 512)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = conn.Read(udpResponse)
		if err != nil {
			return nil, 0, err
		}
		dnsResolve = udpResponse
	}
	respMsg := new(dns.Msg)
	err := respMsg.Unpack(dnsResolve)
	if err != nil {
		return nil, 0, errors.New("failed to unpack dns response for ECH: ", err)
	}
	if len(respMsg.Answer) > 0 {
		for _, answer := range respMsg.Answer {
			if https, ok := answer.(*dns.HTTPS); ok && https.Hdr.Name == dns.Fqdn(domain) {
				for _, v := range https.Value {
					if echConfig, ok := v.(*dns.SVCBECHConfig); ok {
						errors.LogDebug(context.Background(), "Get ECH config:", echConfig.String(), " TTL:", respMsg.Answer[0].Header().Ttl)
						return echConfig.ECH, answer.Header().Ttl, nil
					}
				}
			}
		}
	}
	return nil, dns2.DefaultTTL, dns2.ErrEmptyResponse
}

// reference github.com/OmarTariq612/goech
func MarshalBinary(ech reality.EchConfig) ([]byte, error) {
	var b cryptobyte.Builder
	b.AddUint16(ech.Version)
	b.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
		child.AddUint8(ech.ConfigID)
		child.AddUint16(ech.KemID)
		child.AddUint16(uint16(len(ech.PublicKey)))
		child.AddBytes(ech.PublicKey)
		child.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
			for _, cipherSuite := range ech.SymmetricCipherSuite {
				child.AddUint16(cipherSuite.KDFID)
				child.AddUint16(cipherSuite.AEADID)
			}
		})
		child.AddUint8(ech.MaxNameLength)
		child.AddUint8(uint8(len(ech.PublicName)))
		child.AddBytes(ech.PublicName)
		child.AddUint16LengthPrefixed(func(child *cryptobyte.Builder) {
			for _, extention := range ech.Extensions {
				child.AddUint16(extention.Type)
				child.AddBytes(extention.Data)
			}
		})
	})
	return b.Bytes()
}

var ErrInvalidLen = errors.New("goech: invalid length")

func ConvertToGoECHKeys(data []byte) ([]tls.EncryptedClientHelloKey, error) {
	var keys []tls.EncryptedClientHelloKey
	s := cryptobyte.String(data)
	for !s.Empty() {
		if len(s) < 2 {
			return keys, ErrInvalidLen
		}
		keyLength := int(binary.BigEndian.Uint16(s[:2]))
		if len(s) < keyLength+4 {
			return keys, ErrInvalidLen
		}
		configLength := int(binary.BigEndian.Uint16(s[keyLength+2 : keyLength+4]))
		if len(s) < 2+keyLength+2+configLength {
			return keys, ErrInvalidLen
		}
		child := cryptobyte.String(s[:2+keyLength+2+configLength])
		var (
			sk, config cryptobyte.String
		)
		if !child.ReadUint16LengthPrefixed(&sk) || !child.ReadUint16LengthPrefixed(&config) || !child.Empty() {
			return keys, ErrInvalidLen
		}
		if !s.Skip(2 + keyLength + 2 + configLength) {
			return keys, ErrInvalidLen
		}
		keys = append(keys, tls.EncryptedClientHelloKey{
			Config:     config,
			PrivateKey: sk,
		})
	}
	return keys, nil
}

const ExtensionEncryptedClientHello = 0xfe0d
const KDF_HKDF_SHA384 = 0x0002
const KDF_HKDF_SHA512 = 0x0003

func GenerateECHKeySet(configID uint8, domain string, kem uint16) (reality.EchConfig, []byte, error) {
	config := reality.EchConfig{
		Version:    ExtensionEncryptedClientHello,
		ConfigID:   configID,
		PublicName: []byte(domain),
		KemID:      kem,
		SymmetricCipherSuite: []reality.EchCipher{
			{KDFID: hpke.KDF_HKDF_SHA256, AEADID: hpke.AEAD_AES_128_GCM},
			{KDFID: hpke.KDF_HKDF_SHA256, AEADID: hpke.AEAD_AES_256_GCM},
			{KDFID: hpke.KDF_HKDF_SHA256, AEADID: hpke.AEAD_ChaCha20Poly1305},
			{KDFID: KDF_HKDF_SHA384, AEADID: hpke.AEAD_AES_128_GCM},
			{KDFID: KDF_HKDF_SHA384, AEADID: hpke.AEAD_AES_256_GCM},
			{KDFID: KDF_HKDF_SHA384, AEADID: hpke.AEAD_ChaCha20Poly1305},
			{KDFID: KDF_HKDF_SHA512, AEADID: hpke.AEAD_AES_128_GCM},
			{KDFID: KDF_HKDF_SHA512, AEADID: hpke.AEAD_AES_256_GCM},
			{KDFID: KDF_HKDF_SHA512, AEADID: hpke.AEAD_ChaCha20Poly1305},
		},
		MaxNameLength: 0,
		Extensions:    nil,
	}
	// if kem == hpke.DHKEM_X25519_HKDF_SHA256 {
	curve := ecdh.X25519()
	priv := make([]byte, 32) //x25519
	_, err := io.ReadFull(rand.Reader, priv)
	if err != nil {
		return config, nil, err
	}
	privKey, _ := curve.NewPrivateKey(priv)
	config.PublicKey = privKey.PublicKey().Bytes()
	return config, priv, nil
	// }
	// TODO: add mlkem768 (former kyber768 draft00). The golang mlkem private key is 64 bytes seed?
}
