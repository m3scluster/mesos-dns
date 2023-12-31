// Package resolver contains functions to handle resolving .mesos domains
package resolver

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AVENTER-UG/mesos-dns/exchanger"
	"github.com/AVENTER-UG/mesos-dns/logging"
	"github.com/AVENTER-UG/mesos-dns/models"
	"github.com/AVENTER-UG/mesos-dns/records"
	"github.com/AVENTER-UG/mesos-dns/util"
	restful "github.com/emicklei/go-restful"
	_ "github.com/mesos/mesos-go/api/v0/detector/zoo" // Registers the ZK detector
	"github.com/miekg/dns"
)

// Resolver holds configuration state and the resource records
type Resolver struct {
	masters          []string
	version          string
	config           records.Config
	ready            chan struct{}
	rs               *records.RecordGenerator
	rsLock           sync.RWMutex
	rng              *rand.Rand
	generatorOptions []records.Option
	zoneFwds         map[string]exchanger.Forwarder // map of zone -> forwarder
	defaultFwd       exchanger.Forwarder
}

// New returns a Resolver with the given version and configuration.
func New(version string, config records.Config) *Resolver {
	generatorOptions := []records.Option{
		records.WithConfig(config),
	}
	recordGenerator := records.NewRecordGenerator(generatorOptions...)
	r := &Resolver{
		version: version,
		config:  config,
		ready:   make(chan struct{}),
		rs:      recordGenerator,
		// rand.Sources aren't safe for concurrent use, except the global one.
		// See: https://github.com/golang/go/issues/3611
		rng:              rand.New(&lockedSource{src: rand.NewSource(time.Now().UnixNano())}),
		masters:          append([]string{""}, config.Masters...),
		generatorOptions: generatorOptions,
	}

	timeout := 5 * time.Second
	if config.Timeout != 0 {
		timeout = time.Duration(config.Timeout) * time.Second
	}

	r.zoneFwds = make(map[string]exchanger.Forwarder)
	if config.ExternalOn {
		for zone, resolvers := range config.ZoneResolvers {
			r.zoneFwds[zone] = exchanger.NewForwarder(resolvers, exchangers(timeout, "udp", "tcp"))
		}
		r.defaultFwd = exchanger.NewForwarder(
			config.Resolvers, exchangers(timeout, "udp", "tcp"))
	} else {
		r.defaultFwd = exchanger.NewForwarder(
			make([]string, 0), exchangers(timeout, "udp", "tcp"))
	}

	return r
}

func exchangers(timeout time.Duration, protos ...string) map[string]exchanger.Exchanger {
	exs := make(map[string]exchanger.Exchanger, len(protos))
	for _, proto := range protos {
		exs[proto] = exchanger.Decorate(
			&dns.Client{
				Net:          proto,
				DialTimeout:  timeout,
				ReadTimeout:  timeout,
				WriteTimeout: timeout,
			},
			exchanger.IgnoreErrTruncated,
			exchanger.ErrorLogging(logging.Error),
			exchanger.Instrumentation(
				logging.CurLog.NonMesosForwarded,
				logging.CurLog.NonMesosSuccess,
				logging.CurLog.NonMesosFailed,
			),
		)
	}
	return exs
}

// Ready blocks until the resolver has had a chance to reload at least
// once.
func (res *Resolver) Ready() <-chan struct{} {
	return res.ready
}

// return the current (read-only) record set. attempts to write to the returned
// object will likely result in a data race.
func (res *Resolver) records() *records.RecordGenerator {
	res.rsLock.RLock()
	defer res.rsLock.RUnlock()
	return res.rs
}

// LaunchDNS starts a (TCP and UDP) DNS server for the Resolver,
// returning a error channel to which errors are asynchronously sent.
func (res *Resolver) LaunchDNS() <-chan error {
	// Handers for Mesos requests
	dns.HandleFunc(res.config.Domain+".", panicRecover(res.HandleMesos))

	if res.config.ReverseDNSOn {
		dns.HandleFunc("in-addr.arpa.", panicRecover(res.HandleMesos))
		dns.HandleFunc("ip6.arpa.", panicRecover(res.HandleMesos))
	}

	// Handlers for nonMesos requests
	for zone, fwd := range res.zoneFwds {
		dns.HandleFunc(
			zone+".",
			panicRecover(res.HandleNonMesos(fwd)))
	}
	dns.HandleFunc(
		".",
		panicRecover(res.HandleNonMesos(res.defaultFwd)))

	errCh := make(chan error, 2)
	_, e1 := res.Serve("tcp")
	go func() { errCh <- <-e1 }()
	_, e2 := res.Serve("udp")
	go func() { errCh <- <-e2 }()
	return errCh
}

// Serve starts a DNS server for net protocol (tcp/udp), returns immediately.
// the returned signal chan is closed upon the server successfully entering the listening phase.
// if the server aborts then an error is sent on the error chan.
func (res *Resolver) Serve(proto string) (<-chan struct{}, <-chan error) {
	defer util.HandleCrash()

	ch := make(chan struct{})
	server := &dns.Server{
		Addr:              net.JoinHostPort(res.config.Listener, strconv.Itoa(res.config.Port)),
		Net:               proto,
		TsigSecret:        nil,
		NotifyStartedFunc: func() { close(ch) },
	}

	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		err := server.ListenAndServe()
		if err != nil {
			errCh <- fmt.Errorf("failed to setup %q server: %v", proto, err)
		} else {
			logging.Error.Printf("Not listening/serving any more requests.")
		}
	}()
	return ch, errCh
}

// SetMasters sets the given masters.
// This method is not goroutine-safe.
func (res *Resolver) SetMasters(masters []string) {
	res.masters = masters
}

// Reload triggers a new state load from the configured mesos masters.
// This method is not goroutine-safe.
func (res *Resolver) Reload() {
	t := records.NewRecordGenerator(res.generatorOptions...)
	err := t.ParseState(res.config, res.masters...)

	if err == nil {
		timestamp := uint32(time.Now().Unix())
		// may need to refactor for fairness
		res.rsLock.Lock()
		defer res.rsLock.Unlock()
		atomic.StoreUint32(&res.config.SOASerial, timestamp)
		res.rs = t
		select {
		case <-res.ready:
			// noop because channel is already closed
		default:
			close(res.ready)
		}
	} else {
		logging.Error.Printf("Warning: Error generating records: %v; keeping old DNS state", err)
	}

	logging.PrintCurLog()
}

// formatSRV returns the SRV resource record for target
func (res *Resolver) formatSRV(name string, target string) (*dns.SRV, error) {
	ttl := uint32(res.config.TTL)

	h, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, errors.New("invalid target")
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, errors.New("invalid target port")
	}

	return &dns.SRV{
		Hdr: dns.RR_Header{
			Name:   name,
			Rrtype: dns.TypeSRV,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Priority: 0,
		Weight:   res.config.SRVRecordDefaultWeight,
		Port:     uint16(p),
		Target:   h,
	}, nil
}

// returns the A resource record for target
// assumes target is a well formed IPv4 address
func (res *Resolver) formatA(dom string, target string) (*dns.A, error) {
	ttl := uint32(res.config.TTL)

	a := net.ParseIP(target)
	if a == nil {
		return nil, errors.New("invalid target")
	}

	return &dns.A{
		Hdr: dns.RR_Header{
			Name:   dom,
			Rrtype: dns.TypeA,
			Class:  dns.ClassINET,
			Ttl:    ttl},
		A: a.To4(),
	}, nil
}

// returns the AAAA resource record for target
// assumes target is a well formed IPv6 address
func (res *Resolver) formatAAAA(dom string, target string) (*dns.AAAA, error) {
	ttl := uint32(res.config.TTL)

	aaaa := net.ParseIP(target)
	if aaaa == nil {
		return nil, errors.New("invalid target")
	}

	return &dns.AAAA{
		Hdr: dns.RR_Header{
			Name:   dom,
			Rrtype: dns.TypeAAAA,
			Class:  dns.ClassINET,
			Ttl:    ttl},
		AAAA: aaaa.To16(),
	}, nil
}

// returns the PTR resource record pointing from dom (ip) to ptr
func (res *Resolver) formatPTR(dom string, ptr string) (*dns.PTR, error) {
	ttl := uint32(res.config.TTL)

	return &dns.PTR{
		Hdr: dns.RR_Header{
			Name:   dom,
			Rrtype: dns.TypePTR,
			Class:  dns.ClassINET,
			Ttl:    ttl},
		Ptr: ptr,
	}, nil
}

// formatSOA returns the SOA resource record for the mesos domain
func (res *Resolver) formatSOA(dom string) *dns.SOA {
	ttl := uint32(res.config.TTL)

	return &dns.SOA{
		Hdr: dns.RR_Header{
			Name:   dom,
			Rrtype: dns.TypeSOA,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ns:      res.config.SOAMname,
		Mbox:    res.config.SOARname,
		Serial:  atomic.LoadUint32(&res.config.SOASerial),
		Refresh: res.config.SOARefresh,
		Retry:   res.config.SOARetry,
		Expire:  res.config.SOAExpire,
		Minttl:  ttl,
	}
}

// formatNS returns the NS  record for the mesos domain
func (res *Resolver) formatNS(dom string) *dns.NS {
	ttl := uint32(res.config.TTL)

	return &dns.NS{
		Hdr: dns.RR_Header{
			Name:   dom,
			Rrtype: dns.TypeNS,
			Class:  dns.ClassINET,
			Ttl:    ttl,
		},
		Ns: res.config.SOAMname,
	}
}

// reorders answers for very basic load balancing
func shuffleAnswers(rng *rand.Rand, answers []dns.RR) []dns.RR {
	n := len(answers)
	for i := 0; i < n; i++ {
		r := i + rng.Intn(n-i)
		answers[r], answers[i] = answers[i], answers[r]
	}

	return answers
}

// HandleNonMesos handles non-mesos queries by forwarding to configured
// external DNS servers.
func (res *Resolver) HandleNonMesos(fwd exchanger.Forwarder) func(
	dns.ResponseWriter, *dns.Msg) {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		logging.CurLog.NonMesosRequests.Inc()
		m, err := fwd(r, w.RemoteAddr().Network())
		if err != nil {
			m = new(dns.Msg).SetRcode(r, rcode(err))
		} else if len(m.Answer) == 0 {
			logging.CurLog.NonMesosNXDomain.Inc()
		}
		reply(w, m, res.config.SetTruncateBit)
	}
}

func rcode(err error) int {
	switch err.(type) {
	case *exchanger.ForwardError:
		return dns.RcodeRefused
	default:
		return dns.RcodeServerFailure
	}
}

// HandleMesos is a resolver request handler that responds to a resource
// question with resource answer(s)
// it can handle {A, AAAA, SRV, SOA, NS, ANY}
func (res *Resolver) HandleMesos(w dns.ResponseWriter, r *dns.Msg) {
	logging.CurLog.MesosRequests.Inc()

	m := &dns.Msg{MsgHdr: dns.MsgHdr{
		Authoritative:      true,
		RecursionAvailable: res.config.RecurseOn,
	}}
	m.SetReply(r)

	var errs multiError
	rs := res.records()
	name := strings.ToLower(cleanWild(r.Question[0].Name))
	switch r.Question[0].Qtype {
	case dns.TypeSRV:
		errs.Add(res.handleSRV(rs, name, m, r))
	case dns.TypeA:
		errs.Add(res.handleA(rs, name, m))
	case dns.TypePTR:
		errs.Add(res.handlePTR(rs, name, m))
	case dns.TypeAAAA:
		errs.Add(res.handleAAAA(rs, name, m))
	case dns.TypeSOA:
		errs.Add(res.handleSOA(m, r))
	case dns.TypeNS:
		errs.Add(res.handleNS(m, r))
	case dns.TypeANY:
		errs.Add(
			res.handleSRV(rs, name, m, r),
			res.handleA(rs, name, m),
			res.handleAAAA(rs, name, m),
			res.handleSOA(m, r),
			res.handleNS(m, r),
		)
	}

	if len(m.Answer) == 0 {
		errs.Add(res.handleEmpty(rs, name, m, r))
	} else {
		shuffleAnswers(res.rng, m.Answer)
		logging.CurLog.MesosSuccess.Inc()
	}

	if !errs.Nil() {
		logging.Error.Println(errs.Error())
		logging.CurLog.MesosFailed.Inc()
	}

	reply(w, m, res.config.SetTruncateBit)
}

func (res *Resolver) handlePTR(rs *records.RecordGenerator, name string, m *dns.Msg) error {
	var errs multiError
	ptrs := rs.PTRs[name]
	// > 1 PTR for a given IP implies ambiguity, so, no soup for you!
	if len(ptrs) < 2 {
		for ptr := range ptrs {
			rr, err := res.formatPTR(name, ptr)
			if err != nil {
				errs.Add(err)
				continue
			}
			m.Answer = append(m.Answer, rr)
		}
	}
	return errs
}

func (res *Resolver) handleSRV(rs *records.RecordGenerator, name string, m, r *dns.Msg) error {
	var errs multiError
	aAdded := map[string]struct{}{}    // track the A RR's we've already added, avoid dups
	aaaaAdded := map[string]struct{}{} // track the AAAA RR's we've already added, avoid dups
	for srv := range rs.SRVs[name] {
		srvRR, err := res.formatSRV(r.Question[0].Name, srv)
		if err != nil {
			errs.Add(err)
			continue
		}

		m.Answer = append(m.Answer, srvRR)
		host, _, err := net.SplitHostPort(srv)
		if err != nil {
			logging.Error.Println(err)
		}
		if len(rs.As[host]) == 0 && len(rs.AAAAs[host]) == 0 {
			continue
		}
		if _, aFound := aAdded[host]; !aFound {
			if a, ok := rs.As.First(host); ok {
				if aRR, err := res.formatA(host, a); err == nil {
					m.Extra = append(m.Extra, aRR)
					aAdded[host] = struct{}{}
				} else {
					errs.Add(err)
				}
			}
		}
		if _, aaaaFound := aaaaAdded[host]; !aaaaFound {
			if aaaa, ok := rs.AAAAs.First(host); ok {
				if aaaaRR, err := res.formatAAAA(host, aaaa); err == nil {
					m.Extra = append(m.Extra, aaaaRR)
					aaaaAdded[host] = struct{}{}
				} else {
					errs.Add(err)
				}
			}
		}
	}
	return errs
}

func (res *Resolver) handleA(rs *records.RecordGenerator, name string, m *dns.Msg) error {
	var errs multiError
	for a := range rs.As[name] {
		rr, err := res.formatA(name, a)
		if err != nil {
			errs.Add(err)
			continue
		}
		m.Answer = append(m.Answer, rr)
	}
	return errs
}

func (res *Resolver) handleAAAA(rs *records.RecordGenerator, name string, m *dns.Msg) error {
	var errs multiError
	for aaaa := range rs.AAAAs[name] {
		rr, err := res.formatAAAA(name, aaaa)
		if err != nil {
			errs.Add(err)
			continue
		}
		m.Answer = append(m.Answer, rr)
	}
	return errs
}

func (res *Resolver) handleSOA(m, r *dns.Msg) error {
	m.Ns = append(m.Ns, res.formatSOA(r.Question[0].Name))
	return nil
}

func (res *Resolver) handleNS(m, r *dns.Msg) error {
	m.Ns = append(m.Ns, res.formatNS(r.Question[0].Name))
	return nil
}

func (res *Resolver) handleEmpty(rs *records.RecordGenerator, name string, m, r *dns.Msg) error {
	qType := r.Question[0].Qtype
	switch qType {
	case dns.TypeSOA, dns.TypeNS, dns.TypeSRV:
		logging.CurLog.MesosSuccess.Inc()
		return nil
	}

	m.Rcode = dns.RcodeNameError

	// The second component is just a matter of returning NODATA if we have
	// SRV or A records for the given name, but no neccessarily the given query

	if len(rs.SRVs[name])+len(rs.As[name])+len(rs.AAAAs[name]) > 0 {
		m.Rcode = dns.RcodeSuccess
	}

	logging.CurLog.MesosNXDomain.Inc()
	logging.VeryVerbose.Println("total A rrs:\t" + strconv.Itoa(len(rs.As)))
	logging.VeryVerbose.Println("total AAAA rrs:\t" + strconv.Itoa(len(rs.AAAAs)))
	logging.VeryVerbose.Println("failed looking for " + r.Question[0].String())

	m.Ns = append(m.Ns, res.formatSOA(r.Question[0].Name))

	return nil
}

// reply writes the given dns.Msg out to the given dns.ResponseWriter,
// compressing the message first and truncating it accordingly.
func reply(w dns.ResponseWriter, m *dns.Msg, setTruncateBit bool) {
	m.Compress = true // https://github.com/AVENTER-UG/mesos-dns/issues/{170,173,174}
	maxsize := maxMsgSize(isUDP(w), m.IsEdns0())
	truncate(m, maxsize, setTruncateBit)
	if err := w.WriteMsg(m); err != nil {
		logging.Error.Println(err)
	}
}

// isUDP returns true if the transmission channel in use is UDP.
func isUDP(w dns.ResponseWriter) bool {
	return strings.HasPrefix(w.RemoteAddr().Network(), "udp")
}

// maxMsgSize calculates the maximum size of a DNS message.
// It takes into account whether or not the transport is UDP or TCP.
// In the case of UDP it also checks for the presence of an EDNS0 (OPT)
// record and if found, uses the maximum message size defined by that message.
func maxMsgSize(udp bool, edns *dns.OPT) (size uint16) {
	if !udp {
		// The transport is TCP so we return the maximum message size
		// valid for DNS messages sent over TCP.
		return dns.MaxMsgSize
	}
	// The transport is UDP. By default the maximum message size for DNS
	// messages over UDP is 512 bytes. This is defined by dns.MinMsgSize
	// See 2.3.4 in https://tools.ietf.org/html/rfc1035
	if edns == nil {
		// This message does not specify EDNS0 (OPT) so we use the default
		// maximum size
		return dns.MinMsgSize
	}
	// The EDNS0 (OPT) record is specified in the request and specifies the
	// message maximum size that the requestor can receive.
	return edns.UDPSize()
}

// truncate removes answers until the given dns.Msg fits the permitted
// length of the given transmission channel and sets the TC bit.
// See https://tools.ietf.org/html/rfc1035#section-4.2.1
// If `setTruncateBit` is true, the Truncate bit will be set if the message
// has been truncated already or gets truncated here.
// If `setTruncateBit` is false, the message will have its Truncate bit
// cleared if it is already set, and won't set it even if it does get
// truncated.
func truncate(m *dns.Msg, max uint16, setTruncateBit bool) {
	// truncate() mutates the input to avoid allocating new message objects
	// and incurring the related performance cost.
	furtherTruncation := m.Len() > int(max)
	m.Truncated = m.Truncated || furtherTruncation
	if !setTruncateBit {
		// Clear the Truncate bit regardless of whether this message used
		// to have it set set or whether we are going to truncate it here.
		m.Truncated = false
	}
	if !furtherTruncation {
		return
	}
	// Drop all extra records first
	m.Extra = nil
	if m.Len() < int(max) {
		// Now that the extra records have been dropped, the message size
		// is under the maximum message size limit, so we return it without
		// further modification.
		return
	}
	// The message is still too large. We now truncate the list of answers
	// the message size is smaller than max.
	truncateAnswers(m, max)
}

func truncateAnswers(m *dns.Msg, max uint16) {
	// Store the original list of answers.
	answers := m.Answer
	// Search for the maximum number of answers that can be sent to the
	// client without the DNS message exceeding its maximum allowed size.
	search := func(n int) bool {
		// We shave answers from the back of the slice
		// until we find the point where m.Len is < max.
		// Whatever n is at that point will be returned.
		m.Answer = answers[:len(answers)-n]
		return m.Len() < int(max)
	}
	drop := sort.Search(len(answers), search)
	// drop is the least number of answers that can be removed from the
	// back of the answers slice in order to have m.Len() < max.
	m.Answer = answers[:len(answers)-drop]
}

func (res *Resolver) configureHTTP() {
	// webserver + available routes
	ws := new(restful.WebService)
	ws.Consumes(restful.MIME_JSON)
	ws.Produces(restful.MIME_JSON)

	ws.Route(ws.GET("/v1/version").To(res.RestVersion))
	ws.Route(ws.GET("/v1/config").To(res.RestConfig))
	ws.Route(ws.GET("/v1/hosts/{host}").To(res.RestHost))
	ws.Route(ws.GET("/v1/hosts/{host}/ports").To(res.RestPorts))
	ws.Route(ws.GET("/v1/services/{service}").To(res.RestService))
	if res.config.EnumerationOn {
		ws.Route(ws.GET("/v1/enumerate").To(res.RestEnumerate))
		ws.Route(ws.GET("/v1/axfr").To(res.RestAXFR))
	}
	restful.Add(ws)
}

// LaunchHTTP starts an HTTP server for the Resolver, returning a error channel
// to which errors are asynchronously sent.
func (res *Resolver) LaunchHTTP() <-chan error {
	defer util.HandleCrash()

	res.configureHTTP()
	listenAddress := net.JoinHostPort(res.config.HTTPListener, strconv.Itoa(res.config.HTTPPort))

	errCh := make(chan error, 1)
	go func() {
		var err error
		defer func() { errCh <- err }()

		if err = http.ListenAndServe(listenAddress, nil); err != nil {
			err = fmt.Errorf("failed to setup http server: %v", err)
		} else {
			logging.Error.Println("Not serving http requests any more.")
		}
	}()
	return errCh
}

// RestConfig handles HTTP requests of Resolver configuration.
func (res *Resolver) RestConfig(req *restful.Request, resp *restful.Response) {
	if err := resp.WriteAsJson(res.config); err != nil {
		logging.Error.Println(err)
	}
}

// RestEnumerate handles HTTP requests of the enumeration data
func (res *Resolver) RestEnumerate(req *restful.Request, resp *restful.Response) {

	enumData := res.records().EnumData
	if err := resp.WriteAsJson(enumData); err != nil {
		logging.Error.Println(err)
	}
}

// RestAXFR handles HTTP requests to turn the zone into a transferable format
func (res *Resolver) RestAXFR(req *restful.Request, resp *restful.Response) {
	records := res.records()

	AXFRRecords := models.AXFRRecords{
		SRVs:  records.SRVs.ToAXFRResourceRecordSet(),
		As:    records.As.ToAXFRResourceRecordSet(),
		AAAAs: records.AAAAs.ToAXFRResourceRecordSet(),
	}
	AXFR := models.AXFR{
		Records:        AXFRRecords,
		Serial:         atomic.LoadUint32(&res.config.SOASerial),
		Mname:          res.config.SOAMname,
		Rname:          res.config.SOARname,
		TTL:            res.config.TTL,
		RefreshSeconds: res.config.RefreshSeconds,
		Domain:         res.config.Domain,
	}

	if err := resp.WriteAsJson(AXFR); err != nil {
		logging.Error.Println(err)
	}
}

// RestVersion handles HTTP requests of Mesos-DNS version.
func (res *Resolver) RestVersion(req *restful.Request, resp *restful.Response) {
	err := resp.WriteAsJson(map[string]string{
		"Service": "Mesos-DNS",
		"Version": res.version,
		"URL":     "https://github.com/AVENTER-UG/mesos-dns",
	})
	if err != nil {
		logging.Error.Println(err)
	}
}

// RestHost handles HTTP requests of DNS A records of the given host.
func (res *Resolver) RestHost(req *restful.Request, resp *restful.Response) {
	host := req.PathParameter("host")
	// clean up host name
	dom := strings.ToLower(cleanWild(host))
	if dom[len(dom)-1] != '.' {
		dom += "."
	}
	rs := res.records()

	type record struct {
		Host string `json:"host"`
		IP   string `json:"ip"`
	}

	aRRs := rs.As[dom]
	aaaaRRs := rs.AAAAs[dom]
	records := make([]record, 0, len(aRRs)+len(aaaaRRs))
	for ip := range aRRs {
		records = append(records, record{dom, ip})
	}
	for ip := range aaaaRRs {
		records = append(records, record{dom, ip})
	}

	if len(records) == 0 {
		records = append(records, record{})
	}

	if err := resp.WriteAsJson(records); err != nil {
		logging.Error.Println(err)
	}

	stats(dom, res.config.Domain+".", len(aRRs) > 0)
}

func stats(domain, zone string, success bool) {
	if strings.HasSuffix(domain, zone) {
		logging.CurLog.MesosRequests.Inc()
		if success {
			logging.CurLog.MesosSuccess.Inc()
		} else {
			logging.CurLog.MesosNXDomain.Inc()
		}
	} else {
		logging.CurLog.NonMesosRequests.Inc()
		logging.CurLog.NonMesosFailed.Inc()
	}
}

// RestPorts is an HTTP handler which is currently not implemented.
func (res *Resolver) RestPorts(req *restful.Request, resp *restful.Response) {
	err := resp.WriteErrorString(http.StatusNotImplemented, "To be implemented...")
	if err != nil {
		logging.Error.Println(err)
	}
}

// RestService handles HTTP requests of DNS SRV records for the given name.
func (res *Resolver) RestService(req *restful.Request, resp *restful.Response) {
	service := req.PathParameter("service")
	// clean up service name
	dom := strings.ToLower(cleanWild(service))
	if dom[len(dom)-1] != '.' {
		dom += "."
	}
	rs := res.records()

	type record struct {
		Service string `json:"service"`
		Host    string `json:"host"`
		IP      string `json:"ip"`
		Port    string `json:"port"`
	}

	srvRRs := rs.SRVs[dom]
	records := make([]record, 0, len(srvRRs))
	for s := range srvRRs {
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			logging.Error.Println(err)
			continue
		}
		if aR, aOk := rs.As.First(host); aOk {
			records = append(records, record{service, host, aR, port})
		}
		if aaaaR, aaaaOk := rs.AAAAs.First(host); aaaaOk {
			records = append(records, record{service, host, aaaaR, port})
		}
	}

	if len(records) == 0 {
		records = append(records, record{})
	}

	if err := resp.WriteAsJson(records); err != nil {
		logging.Error.Println(err)
	}

	stats(dom, res.config.Domain+".", len(srvRRs) > 0)
}

// panicRecover catches any panics from the resolvers and sets an error
// code of server failure
func panicRecover(f func(w dns.ResponseWriter, r *dns.Msg)) func(w dns.ResponseWriter, r *dns.Msg) {
	return func(w dns.ResponseWriter, r *dns.Msg) {
		defer func() {
			if rec := recover(); rec != nil {
				m := new(dns.Msg)
				m.SetRcode(r, 2)
				if err := w.WriteMsg(m); err != nil {
					logging.Error.Println(err)
				}
				logging.Error.Println(rec)
			}
		}()
		f(w, r)
	}
}

// cleanWild strips any wildcards out thus mapping cleanly to the
// original serviceName
func cleanWild(name string) string {
	if strings.Contains(name, ".*") {
		return strings.Replace(name, ".*", "", -1)
	}
	return name
}

type multiError []error

func (e *multiError) Add(err ...error) {
	for _, e1 := range err {
		if me, ok := e1.(multiError); ok {
			*e = append(*e, me...)
		} else if e1 != nil {
			*e = append(*e, e1)
		}
	}
}

func (e multiError) Error() string {
	errs := make([]string, len(e))
	for i := range errs {
		if e[i] != nil {
			errs[i] = e[i].Error()
		}
	}
	return strings.Join(errs, "; ")
}

func (e multiError) Nil() bool {
	for _, err := range e {
		if err != nil {
			return false
		}
	}
	return true
}
