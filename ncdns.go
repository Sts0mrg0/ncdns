package main
import "github.com/miekg/dns"
import "github.com/hlandau/degoutils/log"
import "os/signal"
import "os"
import "syscall"
import "fmt"
import "strings"
import "sort"
import "github.com/hlandau/degoutils/config"
import "github.com/hlandau/ncdns/ncerr"

// A Go daemon to serve Namecoin domain records via DNS.
// This daemon is intended to be used in one of the following situations:
//
// 1. It is desired to mirror a domain name suffix (bit.suffix) to the .bit TLD.
//    Accordingly, bit.suffix is delegated to one or more servers each running this daemon.
//
// 2. It is desired to act as an authoritative server for the .bit TLD directly.
//    For example, a recursive DNS resolver is configured to override the root zone and use
//    a server running this daemon for .bit. Or .bit is added to the root zone (when pigs fly).
//
//    If the Unbound recursive DNS resolver were used:
//      unbound.conf:
//        server:
//          stub-zone:
//            name: bit
//            stub-addr: 127.0.0.1@1153
//
// This daemon currently requires namecoind or a compatible daemon running with JSON-RPC interface.
// The name_* API calls are used to obtain .bit domain information.

func main() {
  cfg := ServerConfig {}
  config := config.Configurator{
    ProgramName: "ncdns",
    ConfigFilePaths: []string { "etc/ncdns.conf", "/etc/ncdns/ncdns.conf", },
  }
  config.ParseFatal(&cfg)
  s := NewServer(&cfg)
  s.Run()
}

func NewServer(cfg *ServerConfig) *Server {
  s := &Server{}
  s.cfg = *cfg
  return s
}

func (s *Server) loadKey(fn, privateFn string) (k *dns.DNSKEY, privatek dns.PrivateKey, err error) {
  f, err := os.Open(fn)
  if err != nil {
    return
  }

  rr, err := dns.ReadRR(f, fn)
  if err != nil {
    return
  }

  k, ok := rr.(*dns.DNSKEY)
  if !ok {
    err = fmt.Errorf("Loaded record from key file, but it wasn't a DNSKEY")
    return
  }

  privatef, err := os.Open(privateFn)
  if err != nil {
    return
  }

  privatek, err = k.ReadPrivateKey(privatef, privateFn)
  log.Fatale(err)

  return
}

func (s *Server) Run() {
  var err error

  s.mux = dns.NewServeMux()
  s.mux.HandleFunc(".", s.handle)

  // key setup
  s.ksk, s.kskPrivate, err = s.loadKey(s.cfg.PublicKey, s.cfg.PrivateKey)
  log.Fatale(err, "error reading KSK key")

  if s.cfg.ZonePublicKey != "" {
    s.zsk, s.zskPrivate, err = s.loadKey(s.cfg.ZonePublicKey, s.cfg.ZonePrivateKey)
    log.Fatale(err, "error reading ZSK key")
  } else {
    s.zsk = &dns.DNSKEY{}
    s.zsk.Hdr.Rrtype = dns.TypeDNSKEY
    s.zsk.Hdr.Class  = dns.ClassINET
    s.zsk.Hdr.Ttl    = 3600
    s.zsk.Algorithm = dns.RSASHA256
    s.zsk.Protocol = 3
    s.zsk.Flags = dns.ZONE

    s.zskPrivate, err = s.zsk.Generate(2048)
    log.Fatale(err)
  }

  s.b, err = NewNCBackend(s)
  log.Fatale(err)

  // run
  s.udpListener = s.runListener("udp")
  s.tcpListener = s.runListener("tcp")

  log.Info("Ready.")

  // wait
  sig := make(chan os.Signal)
  signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
  for {
    s := <-sig
    fmt.Printf("Signal %v received, stopping.", s)
    break
  }
}

type Server struct {
  mux *dns.ServeMux
  udpListener *dns.Server
  tcpListener *dns.Server
  ksk *dns.DNSKEY
  kskPrivate dns.PrivateKey
  zsk *dns.DNSKEY
  zskPrivate dns.PrivateKey
  cfg ServerConfig
  b Backend
}

type ServerConfig struct {
  Bind string         `default:":53" usage:"Address to bind to (e.g. 0.0.0.0:53)"`
  PublicKey string    `default:"ncdns.key" usage:"Path to the DNSKEY KSK public key file"`
  PrivateKey string   `default:"ncdns.private" usage:"Path to the KSK's corresponding private key file"`
  ZonePublicKey string `default:"" usage:"Path to the DNSKEY ZSK public key file; if one is not specified, a temporary one is generated on startup and used only for the duration of that process"`
  ZonePrivateKey string `default:"" usage:"Path to the ZSK's corresponding private key file"`

  NamecoinRPCUsername string `default:"" usage:"Namecoin RPC username"`
  NamecoinRPCPassword string `default:"" usage:"Namecoin RPC password"`
  NamecoinRPCAddress  string `default:"localhost:8336" usage:"Namecoin RPC server address"`
  CacheMaxEntries     int    `default:"1000" usage:"Maximum name cache entries"`
  SelfIP              string `default:"127.127.127.127" usage:"The canonical IP address for this service"`
  SelfName            string `default:"" usage:"Canonical name for this nameserver (default: autogenerated psuedo-hostname resolving to SelfIP; SelfIP is not used if this is set)"`
}

func (s *Server) doRunListener(ds *dns.Server) {
  err := ds.ListenAndServe()
  log.Fatale(err)
}

func (s *Server) runListener(net string) *dns.Server {
  ds := &dns.Server {
    Addr: s.cfg.Bind,
    Net: net,
    Handler: s.mux,
  }
  go s.doRunListener(ds)
  return ds
}

type Tx struct {
  req *dns.Msg
  res *dns.Msg
  qname  string
  qtype  uint16
  qclass uint16
  s      *Server
  rcode  int

  typesAtQname map[uint16]struct{}
  additionalQueue map[string]struct{}
  soa *dns.SOA
  delegationPoint string // domain name at which the selected delegation was found

  // The query was made for the selected delegation's name.
  // i.e., if a lookup a.b.c.d has been made, and b.c.d  has been chosen as the
  // closest available delegation to serve, this is false. Whereas if b.c.d is
  // queried, this is true.
  queryIsAtDelegationPoint bool

  // Add a 'consolation SOA' to the Authority section?
  // Usually set when there are no results. This has to be done later, because
  // we add DNSKEYs (if requested) at a later time and need to be able to quash
  // this at that time in case adding DNSKEYs means an answer has stopped being
  // empty of results.
  consolationSOA bool

  // Don't NSEC for having no answers. Used for qtype==DS.
  suppressNSEC bool
}

func (s *Server) handle(rw dns.ResponseWriter, reqMsg *dns.Msg) {
  tx := Tx{}
  tx.req = reqMsg
  tx.res = &dns.Msg{}
  tx.res.SetReply(tx.req)
  tx.res.Authoritative = true
  tx.res.Compress = true
  tx.s = s
  tx.rcode = 0
  tx.typesAtQname = map[uint16]struct{}{}
  tx.additionalQueue = map[string]struct{}{}

  opt := tx.req.IsEdns0()
  if opt != nil {
    tx.res.Extra = append(tx.res.Extra, opt)
  }

  for _, q := range tx.req.Question {
    tx.qname  = strings.ToLower(q.Name)
    tx.qtype  = q.Qtype
    tx.qclass = q.Qclass

    if q.Qclass != dns.ClassINET && q.Qclass != dns.ClassANY {
      continue
    }

    err := tx.addAnswers()
    if err != nil {
      if err == ncerr.ErrNoResults {
        tx.rcode = 0
      } else if err == ncerr.ErrNoSuchDomain {
        tx.rcode = dns.RcodeNameError
      } else if tx.rcode == 0 {
        log.Infoe(err, "Handler error, doing SERVFAIL")
        tx.rcode = dns.RcodeServerFailure
      }
      break
    }

  }

  tx.res.SetRcode(tx.req, tx.rcode)

  //log.Info("response: ", res.String())
  err := rw.WriteMsg(tx.res)
  log.Infoe(err, "Couldn't write response: " + tx.res.String())
}

func (tx *Tx) blookup(qname string) (rrs []dns.RR, err error) {
  log.Info("blookup: ", qname)
  rrs, err = tx.s.b.Lookup(qname)
  if err == nil && len(rrs) == 0 {
    err = ncerr.ErrNoResults
  }
  return
}


func rrsetHasType(rrs []dns.RR, t uint16) dns.RR {
  for i := range rrs {
    if rrs[i].Header().Rrtype == t {
      return rrs[i]
    }
  }
  return nil
}

func (tx *Tx) addAnswers() error {
  err := tx.addAnswersMain()
  if err != nil {
    return err
  }

  // If we are at the zone apex...
  if _, ok := tx.typesAtQname[dns.TypeSOA]; tx.soa != nil && ok {
    // Add DNSKEYs.
    if tx.istype(dns.TypeDNSKEY) {
      tx.s.ksk.Hdr.Name = tx.soa.Hdr.Name
      tx.s.zsk.Hdr.Name = tx.s.ksk.Hdr.Name

      tx.res.Answer = append(tx.res.Answer, tx.s.ksk)
      tx.res.Answer = append(tx.res.Answer, tx.s.zsk)

      // cancel sending a consolation SOA since we're giving DNSKEY answers
      tx.consolationSOA = false
    }

    tx.typesAtQname[dns.TypeDNSKEY] = struct{}{}
  }

  //
  if tx.consolationSOA && tx.soa != nil {
    tx.res.Ns = append(tx.res.Ns, tx.soa)
  }

  err = tx.addNSEC()
  if err != nil {
    return err
  }

  err = tx.addAdditional()
  if err != nil {
    return err
  }

  err = tx.signResponse()
  if err != nil {
    return err
  }

  return nil
}

func (tx *Tx) addAnswersMain() error {
  var soa *dns.SOA
  var origq []dns.RR
  var origerr error
  var firsterr error
  var nss []dns.RR
  firstNSAtLen := -1
  firstSOAAtLen := -1

  // We have to find out the zone root by trying to find SOA for progressively shorter domain names.
  norig := strings.TrimRight(tx.qname, ".")
  n := norig

A:
  for len(n) > 0 {
    rrs, err := tx.blookup(n)
    if len(n) == len(norig) { // keep track of the results for the original qname
      origq = rrs
      origerr = err
    }
    if err == nil { // success
      for i := range rrs {
        t := rrs[i].Header().Rrtype
        switch t {
          case dns.TypeSOA:
            // found the apex of the closest zone for which we are authoritative
            // We haven't found any nameservers at this point, so we can serve without worrying about delegations.
            if soa == nil {
              soa = rrs[i].(*dns.SOA)
            }

            // We have found a SOA record at this level. This is preferred over everything
            // so we can break now.
            if firstSOAAtLen < 0 {
              firstSOAAtLen = len(n)
            }
            break A

          case dns.TypeNS:
            // found an NS on the path; we are not authoritative for this owner or anything under it
            // We need to return Authority data regardless of the nature of the query.
            nss = rrs

            // There could also be a SOA record at this level that we haven't reached yet.
            if firstNSAtLen < 0 {
              firstNSAtLen = len(n)

              tx.delegationPoint = absname(n)
              log.Info("DELEGATION POINT: ", tx.delegationPoint)

              if n == norig {
                tx.queryIsAtDelegationPoint = true
              }
            }

          default:
        }
      }
    } else if firsterr == nil {
      firsterr = err
    }

    nidx := strings.Index(n, ".")
    if nidx < 0 {
      break
    }
    n = n[nidx+1:]
  }

  if soa == nil {
    // If we didn't even get a SOA at any point, we don't have any appropriate zone for this query.
    return ncerr.ErrNotInZone
  }

  tx.soa = soa

  if firstSOAAtLen >= firstNSAtLen {
    // We got a SOA and zero or more NSes at the same level; we're not a delegation.
    return tx.addAnswersAuthoritative(origq, origerr)
  } else {
    // We have a delegation.
    return tx.addAnswersDelegation(nss)
  }
}

func (tx *Tx) addAnswersAuthoritative(rrs []dns.RR, origerr error) error {
  log.Info("AUTHORITATIVE")

  // A call to blookup either succeeds or fails.
  //
  // If it fails:
  //   ErrNotInZone     -- you're looking fundamentally in the wrong place; if there is no other
  //                       appropriate zone, fail with REFUSED
  //   ErrNoSuchDomain  -- there are no records at this name of ANY type, nor are there at any
  //                       direct or indirect descendant domain; fail with NXDOMAIN
  //   ErrNoResults     -- There are no records of the given type of class. However, there are
  //                       other records at the given domain and/or records at a direct or
  //                       indirect descendant domain; NOERROR
  //   any other error  -- SERVFAIL
  //
  // If it succeeds:
  //   If there are zero records, treat the response as ErrNoResults above. Otherwise, each record
  //   can be classified into one of the following categories:
  //
  //     - A NS record not at the zone apex and thus not authoritative (handled in addAnswersDelegation)
  //
  //     - A record not within the zone and thus not authoritative (glue records)
  //
  //     - A CNAME record (must not be glue) (TODO: DNAME)
  //
  //     - Any other record
  if origerr != nil {
    return origerr
  }

  cn := rrsetHasType(rrs, dns.TypeCNAME)
  if cn != nil && !tx.istype(dns.TypeCNAME) {
    // We have an alias.
    // TODO: check that the CNAME record is actually in the zone and not some bizarro CNAME glue record
    return tx.addAnswersCNAME(cn.(*dns.CNAME))
  }

  // Add every record which was requested.
  for i := range rrs {
    t := rrs[i].Header().Rrtype
    if tx.istype(t) {
      tx.res.Answer = append(tx.res.Answer, rrs[i])
    }

    // Keep track of the types that really do exist here in case we have to NSEC.
    tx.typesAtQname[t] = struct{}{}
  }

  if len(tx.res.Answer) == 0 {
    // no matching records, hand out the SOA (done later, might be quashed)
    tx.consolationSOA = true
  }

  return nil
}

func (tx *Tx) addAnswersCNAME(cn *dns.CNAME) error {
  tx.res.Answer = append(tx.res.Answer, cn)
  return nil
}

func (tx *Tx) addAnswersDelegation(nss []dns.RR) error {
  log.Info("DELEGATION")

  if tx.qtype == dns.TypeDS /* don't use istype, must not match ANY */ &&
     tx.queryIsAtDelegationPoint {
    // If type DS was requested specifically (not ANY), we have to act like
    // we're handling things authoritatively and hand out a consolation SOA
    // record and NOT hand out NS records. These still go in the Authority
    // section though.
    //
    // If a DS record exists, it's given; if one doesn't, an NSEC record is
    // given.
    added := false
    for _, ns := range nss {
      t := ns.Header().Rrtype
      if t == dns.TypeDS {
        added = true
        tx.res.Answer = append(tx.res.Answer, ns)
      }
    }
    if added {
      tx.suppressNSEC = true
    } else {
      tx.consolationSOA = true
    }
  } else {
    tx.res.Authoritative = false

    // Note that this is not authoritative data and thus does not get signed.
    for _, ns := range nss {
      t := ns.Header().Rrtype
      if t == dns.TypeNS || t == dns.TypeDS {
        tx.res.Ns = append(tx.res.Ns, ns)
      }
      if t == dns.TypeNS {
        ns_ := ns.(*dns.NS)
        tx.queueAdditional(ns_.Ns)
      }
      if t == dns.TypeDS {
        tx.suppressNSEC = true
      }
    }
  }

  // Nonauthoritative NS records are still included in the NSEC extant types list
  tx.typesAtQname[dns.TypeNS] = struct{}{}

  return nil
}

func (tx *Tx) queueAdditional(name string) {
  tx.additionalQueue[name] = struct{}{}
}

func (tx *Tx) addNSEC() error {
  if !tx.useDNSSEC() || tx.suppressNSEC {
    return nil
  }

  // NSEC replies should be given in the following circumstances:
  //
  //   - No ANSWER SECTION responses for type requested, qtype != DS
  //   - No ANSWER SECTION responses for type requested, qtype == DS
  //   - Wildcard, no data responses
  //   - Wildcard, data response
  //   - Name error response
  //   - Direct NSEC request

  if len(tx.res.Answer) == 0 {
    log.Info("adding NSEC3")
    err := tx.addNSEC3RR()
    if err != nil {
      return err
    }
  }

  return nil
}

func (tx *Tx) addNSEC3RR() error {
  // deny the name
  err := tx.addNSEC3RRActual(tx.qname, tx.typesAtQname)
  if err != nil {
    return err
  }

  // DEVEVER.BIT.
  // deny DEVEVER.BIT. (DS)
  // deny *.BIT.

  // deny the existence of a wildcard that could have served the name

  return nil
}

func (tx *Tx) addNSEC3RRActual(name string, tset map[uint16]struct{}) error {
  tbm := []uint16{}
  for t, _ := range tset {
    tbm = append(tbm, t)
  }

  sort.Sort(uint16Slice(tbm))

  nsr1n  := dns.HashName(tx.qname, dns.SHA1, 1, "8F")
  nsr1nn := stepName(nsr1n)
  nsr1   := &dns.NSEC3 {
    Hdr: dns.RR_Header {
      Name: absname(nsr1n + "." + tx.soa.Hdr.Name),
      Rrtype: dns.TypeNSEC3,
      Class: dns.ClassINET,
      Ttl: 600,
    },
    Hash: dns.SHA1,
    Flags: 0,
    Iterations: 1,
    SaltLength: 1,
    Salt: "8F",
    HashLength: uint8(len(nsr1nn)),
    NextDomain: nsr1nn,
    TypeBitMap: tbm,
  }
  tx.res.Ns = append(tx.res.Ns, nsr1)

  return nil
}

func (tx *Tx) addAdditional() error {
  for aname := range tx.additionalQueue {
    err := tx.addAdditionalItem(aname)
    if err != nil {
      // eat the error
      //return err
    }
  }
  return nil
}

func (tx *Tx) addAdditionalItem(aname string) error {
  log.Info("ADDITIONAL:  ", aname)
  rrs, err := tx.blookup(aname)
  if err != nil {
    return err
  }
  for _, rr := range rrs {
    t := rr.Header().Rrtype
    if t == dns.TypeA || t == dns.TypeAAAA {
      tx.res.Extra = append(tx.res.Extra, rr)
    }
  }
  return nil
}

// © 2014 Hugo Landau <hlandau@devever.net>      GPLv3 or later
