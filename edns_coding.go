package main

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v2"

	"github.com/codegangsta/cli"
	"github.com/snowsky/dns"
)

type ZonesConf struct {
	Zones map[string]map[string]map[string][]string
}

var ZonesFile ZonesConf

type ZoneDetails struct {
	zones     allZones
	zonesConf ZonesConf
}

type allZones map[string]map[uint16][]dns.RR

func (zs *ZoneDetails) serveZones() error {
	dns.HandleFunc(".", zs.dnsReqHandler)
	dnsServer := &dns.Server{Addr: ":8053", Net: "udp"}
	err := dnsServer.ListenAndServe()
	return err
}

func (zs *ZoneDetails) dnsReqHandler(rw dns.ResponseWriter, m *dns.Msg) {
	fmt.Println("Dealing with DNS requests...")

	dnsMsg := new(dns.Msg)
	dnsMsg.SetReply(m)
	q := &m.Question[0]

	// fmt.Println("Question: ", q.Qtype, zs.zones[q.Name][dns.TypePROXY])
	if zs.zones[q.Name][dns.TypePROXY] != nil {
		zs.askProxyNS(rw, m)
	} else {
		allRecords, present := zs.zones[q.Name]
		if !present {
			dnsMsg.Rcode = dns.RcodeNameError
			rw.WriteMsg(dnsMsg)
			return
		}

		qRecords, present := allRecords[q.Qtype]
		if !present {
			dnsMsg.Rcode = dns.RcodeNXRrset
			rw.WriteMsg(dnsMsg)
			return
		}

		dnsMsg.Answer = append(dnsMsg.Answer, qRecords...)
		rw.WriteMsg(dnsMsg)
		return
	}
}

// 先从第一个answer中找到CNAME，然后发送EDNS请求到NS来获得地址，并返回
func (zs *ZoneDetails) askProxyNS(rw dns.ResponseWriter, m *dns.Msg) {
	dnsMsg1 := new(dns.Msg)
	dnsMsg1.Question = make([]dns.Question, 1)
	q := &m.Question[0]
	keys := reflect.ValueOf(zs.zonesConf.Zones).MapKeys()
	zoneName := keys[0].String()
	nsProxy := zs.zonesConf.Zones[zoneName][strings.Trim(q.Name, ".")]["proxy"]

	allRecords, present := zs.zones[q.Name]
	if !present {
		dnsMsg1.Rcode = dns.RcodeNameError
		rw.WriteMsg(dnsMsg1)
		return
	}

	qRecords, present := allRecords[dns.TypePROXY]
	if !present {
		dnsMsg1.Rcode = dns.RcodeNXRrset
		rw.WriteMsg(dnsMsg1)
		return
	}

	dnsMsg1.Answer = append(dnsMsg1.Answer, qRecords...)

	for _, answer := range dnsMsg1.Answer {
		fmt.Println(strings.Split(answer.String(), "\t")[0], rw.RemoteAddr().String())

		conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// 原型只尝试第一个DNS服务器
		nsResolv := conf.Servers[0]
		// fmt.Println(dns.Fqdn(strings.Split(answer.String(), "\t")[0]))
		dnsMsg1.Question[0] = dns.Question{dns.Fqdn(strings.Split(answer.String(), "\t")[0]), dns.TypeA, uint16(dns.ClassINET)}
		dnsMsg1.Id = m.Id
		dnsMsg1.Opcode = dns.OpcodeQuery
		dnsMsg1.MsgHdr.RecursionDesired = true

		c := new(dns.Client)
		c.Net = "udp"
		nsResolv = net.JoinHostPort(nsResolv, strconv.Itoa(53))
		in, _, err := c.Exchange(dnsMsg1, nsResolv)
		if err != nil {
			fmt.Println(err)
			continue
		}
		in.Id = dnsMsg1.Id
		// fmt.Printf("%v %s", in, rtt)
		var realFQDN string
		for _, answer := range in.Answer {
			fmt.Println(strings.Split(answer.String(), "\t")[3], qRecords[0])

			if strings.Split(answer.String(), "\t")[3] == "CNAME" {
				realFQDN = strings.Split(answer.String(), "\t")[4]
			}
		}

		// 准备发送EDNS请求，带上clinet_subnet参数的DNS请求
		dnsMsg2 := new(dns.Msg)
		dnsMsg2.Question = make([]dns.Question, 1)
		dnsMsg2.Question[0] = dns.Question{dns.Fqdn(realFQDN), dns.TypeA, uint16(dns.ClassINET)}
		dnsMsg2.Opcode = dns.OpcodeQuery
		dnsMsg2.MsgHdr.RecursionDesired = true

		o := new(dns.OPT)
		o.Hdr.Name = "."
		o.Hdr.Rrtype = dns.TypeOPT
		e := new(dns.EDNS0_SUBNET)
		e.Code = dns.EDNS0SUBNET
		e.SourceScope = 0
		e.Address = net.ParseIP(strings.Split(rw.RemoteAddr().String(), ":")[0])
		if e.Address == nil {
			fmt.Fprintf(os.Stderr, "Failure to parse IP address: %s\n", e.Address)
			return
		}
		e.Family = 1 // IP4
		e.SourceNetmask = net.IPv4len * 8
		o.Option = append(o.Option, e)
		dnsMsg2.Extra = append(m.Extra, o)

		// TODO: (snowsky) Will use goroutines later
		for _, ns := range nsProxy {
			ns = net.JoinHostPort(ns, strconv.Itoa(53))
			secondAnswer, rtt, err := c.Exchange(dnsMsg2, ns)
			if err != nil {
				fmt.Println(err, rtt)
				continue
			}
			secondAnswer.Id = dnsMsg1.Id
			secondAnswer.Question = dnsMsg1.Question
			fmt.Println(secondAnswer)
			rw.WriteMsg(secondAnswer)
			return
		}
	}
}

func apiReload(w http.ResponseWriter, r *http.Request) {
	fmt.Println("API reloaded!")
}

func buildZones(zonesconf ZonesConf) (allZones, error) {
	z := make(allZones)
	for _, hosts := range zonesconf.Zones {
		for host, records := range hosts {
			host = dns.Fqdn(host)
			if _, existing := z[host]; !existing {
				z[host] = make(map[uint16][]dns.RR)
			}

			for typeStr, values := range records {
				rtype, existing := dns.StringToType[strings.ToUpper(typeStr)]
				if !existing {
					return nil, fmt.Errorf("Invalid record type")
				}

				for _, result := range values {
					rr, err := dns.NewRR(fmt.Sprintf("%s %s %s", host, strings.ToUpper(typeStr), result))
					if err != nil {
						return nil, fmt.Errorf("Couldn't parse record: %v", err)
					}
					z[host][rtype] = append(z[host][rtype], rr)
				}
			}
		}
	}
	return z, nil
}

func loadConf(filename string) (ZonesConf, error) {
	zs := ZonesConf{}
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		return zs, nil
	}
	err = yaml.Unmarshal(content, &zs)
	return zs, err
}

func main() {
	fmt.Println("Starting EDNS Server App...")

	opt := cli.NewApp()
	opt.Name = "dns_server"
	opt.Usage = "Support simple and Extensive DNS queries"
	opt.Version = "0.0.1"

	opt.Flags = []cli.Flag{}
	opt.Commands = []cli.Command{
		{
			Name:  "start",
			Usage: "Start DNS server",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "conf",
					Usage: "configuration file",
				},
				cli.StringFlag{
					Name:  "reload-api",
					Value: "127.0.0.1:8054",
				},
			},
			Action: func(c *cli.Context) {
				go func() {
					http.HandleFunc("/dns/reload", apiReload)
				}()
				if conf := c.String("conf"); conf != "" {
					ZonesFile, _ = loadConf(conf)

					zs, err := buildZones(ZonesFile)
					if err != nil {
						fmt.Println("Failed to build zones!", err)
						return
					}
					fmt.Println(zs)

					zoneDetails := ZoneDetails{
						zones:     zs,
						zonesConf: ZonesFile,
					}
					err = zoneDetails.serveZones()
					if err != nil {
						fmt.Println("Failed to start DNS service!")
						os.Exit(1)
					}
				} else {
					fmt.Println("Please give me the conf file!")
					return
				}

				sig := make(chan os.Signal)
				signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

			forever:
				for {
					select {
					case s := <-sig:
						fmt.Printf("Signal (%v) received!", s)
						break forever
					}
				}
			},
		},
		{
			Name:  "reload",
			Usage: "Reload DNS server",
			Flags: []cli.Flag{},
		},
	}

	err := opt.Run(os.Args)
	if err != nil {
		fmt.Println(err)
	}
}
