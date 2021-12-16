package main

import (
	"container/list"
	"context"
	"flag"
	"fmt"
	"github.com/EDDYCJY/fake-useragent"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"net"

	URL "net/url"

	"github.com/apoorvam/goterminal"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	netstat "github.com/shirou/gopsutil/net"
)

const (
	letterIdxBits = 6
	letterIdxMask = 1<<letterIdxBits - 1
	letterIdxMax  = 63 / letterIdxBits
)

type speedPair struct {
	index uint64
	speed float64
}

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

var SpeedQueue = list.New()
var SpeedIndex uint64 = 0

type ipArray []string

func (i *ipArray) String() string {
	return strings.Join(*i, ",")
}

func (i *ipArray) Set(value string) error {
	*i = append(*i, strings.TrimSpace(value))
	return nil
}

func RandStringBytesMaskImpr(n int) string {
	b := make([]byte, n)
	for i, cache, remain := n-1, rand.Int63(), letterIdxMax; i >= 0; {
		if remain == 0 {
			cache, remain = rand.Int63(), letterIdxMax
		}
		if idx := int(cache & letterIdxMask); idx < len(letterBytes) {
			b[i] = letterBytes[idx]
			i--
		}
		cache >>= letterIdxBits
		remain--
	}
	return string(b)
}

func generateRandomIPAddress() string {
	rand.Seed(time.Now().Unix())
	ip := fmt.Sprintf("%d.%d.%d.%d", rand.Intn(255), rand.Intn(255), rand.Intn(255), rand.Intn(255))
	return ip
}

func LeastSquares(x []float64, y []float64) (a float64, b float64) {
	xi := float64(0)
	x2 := float64(0)
	yi := float64(0)
	xy := float64(0)
	if len(x) != len(y) {
		a = 0
		b = 0
		return
	} else {
		length := float64(len(x))
		for i := 0; i < len(x); i++ {
			xi += x[i]
			x2 += x[i] * x[i]
			yi += y[i]
			xy += x[i] * y[i]
		}
		a = (yi*xi - xy*length) / (xi*xi - x2*length)
		b = (yi*x2 - xy*xi) / (x2*length - xi*xi)
	}
	return
}

func showStat() {

	initialNetCounter, _ := netstat.IOCounters(true)
	iplist := ""
	if customIP !=nil && len(customIP)>0{
		iplist = customIP.String()
	}else{
		u, _ := URL.Parse(*url)
		iplist = strings.Join(nslookup(u.Hostname(),"8.8.8.8"),",")
	}

	for true {
		percent, _ := cpu.Percent(time.Second, false)
		memStat, _ := mem.VirtualMemory()
		netCounter, _ := netstat.IOCounters(true)
		loadStat, _ := load.Avg()

		fmt.Fprintf(TerminalWriter, "target URL:%s\n", *url)
		fmt.Fprintf(TerminalWriter, "target IP:%s\n", iplist)

		fmt.Fprintf(TerminalWriter, "cpu percent:%.3f%% \n", percent)
		fmt.Fprintf(TerminalWriter, "mem percent:%.3f%% \n", memStat.UsedPercent)
		fmt.Fprintf(TerminalWriter, "load info:%.3f %.3f %.3f\n", loadStat.Load1, loadStat.Load5, loadStat.Load15)
		for i := 0; i < len(netCounter); i++ {
			if netCounter[i].BytesRecv == 0 && netCounter[i].BytesSent == 0 {
				continue
			}
			RecvBytes := float64(netCounter[i].BytesRecv - initialNetCounter[i].BytesRecv)
			SendBytes := float64(netCounter[i].BytesSent - initialNetCounter[i].BytesSent)
			//if RecvBytes > 1000 {
			//	SpeedIndex++
			//	pair := speedPair{
			//		index: SpeedIndex,
			//		speed: RecvBytes,
			//	}
			//	SpeedQueue.PushBack(pair)
			//	if SpeedQueue.Len() > 60 {
			//		SpeedQueue.Remove(SpeedQueue.Front())
			//	}
			//	var x []float64
			//	var y []float64
			//	x = make([]float64, 60)
			//	y = make([]float64, 60)
			//	var point = 0
			//	for item := SpeedQueue.Front(); item != nil; item = item.Next() {
			//		spdPair := item.Value.(speedPair)
			//		x[point] = float64(spdPair.index)
			//		y[point] = spdPair.speed
			//		point++
			//	}
			//	_, b := LeastSquares(x, y)
			//	log.Printf("Speed Vertical:%.3f\n", b)
			//}
			fmt.Fprintf(TerminalWriter, "Nic:%v,Recv %s/s,Send %s/s\n", netCounter[i].Name,
				readableBytes(RecvBytes),
				readableBytes(SendBytes))
		}
		initialNetCounter = netCounter
		TerminalWriter.Clear()
		TerminalWriter.Print()
		time.Sleep(1 * time.Millisecond)
	}
}

func readableBytes(bytes float64) (expression string) {
	if bytes == 0 {
		return "0B"
	}
	var i = math.Floor(math.Log(bytes) / math.Log(1024))
	var sizes = []string{"B", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}
	return fmt.Sprintf("%.3f%s", bytes/math.Pow(1024, i), sizes[int(i)])
}

func nslookup(targetAddress, server string) (res []string) {
	if server == "" {
		server = "8.8.8.8"
	}
	c := dns.Client{}
	m := dns.Msg{}
	m.SetQuestion(targetAddress+".", dns.TypeA)

	ns := server + ":53"
	r, t, err := c.Exchange(&m, ns)
	if err != nil {
		fmt.Printf("nameserver %s error: %v\n", ns, err)
		return res
	}
	fmt.Printf("nameserver %s took %v", ns, t)
	if len(r.Answer) == 0 {
		return res
	}
	for _, ans := range r.Answer {
		Arecord := ans.(*dns.A)
		res = append(res, fmt.Sprintf("%s", Arecord))
	}
	return
}

func goFun(Url string, postContent string, Referer string, XforwardFor bool, customIP ipArray, wg *sync.WaitGroup) {
	defer func() {
		if r := recover(); r != nil {
			go goFun(Url, postContent, Referer, XforwardFor, customIP, wg)
		}
	}()
	for true {
		if customIP != nil && len(customIP) > 0 {
			dialer := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			http.DefaultTransport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				rand.Seed(time.Now().Unix())
				ip := customIP[rand.Intn(len(customIP))]
				if strings.HasPrefix(addr,"https"){
					addr = ip + ":443"
				} else if strings.HasPrefix(addr,"http") {
					addr = ip + ":80"
				} else {
					addr = ip + ":80"
				}
				return dialer.DialContext(ctx, network, addr)
			}
		}

		var request *http.Request
		var err1 error = nil
		client := &http.Client{}
		if len(postContent) > 0 {
			request, err1 = http.NewRequest("POST", Url, strings.NewReader(postContent))
		} else {
			request, err1 = http.NewRequest("GET", Url, nil)
		}
		if err1 != nil {
			continue
		}
		if len(Referer) == 0 {
			Referer = Url
		}
		request.Header.Add("Cookie", RandStringBytesMaskImpr(12))
		request.Header.Add("User-Agent", browser.Random())
		request.Header.Add("Referer", Referer)
		if XforwardFor {
			randomip := generateRandomIPAddress()
			request.Header.Add("X-Forwarded-For", randomip)
			request.Header.Add("X-Real-IP", randomip)
		}

		resp, err2 := client.Do(request)
		if err2 != nil {
			continue
		}
		_, err3 := io.Copy(ioutil.Discard, resp.Body)
		if err3 != nil {
			continue
		}
	}
	wg.Done()
}

var count = flag.Int("c", 16, "concurrent thread for download,default 8")
var url = flag.String("s", "https://baidu.com", "target url")
var postContent = flag.String("p", "", "post content")
var referer = flag.String("r", "", "referer url")
var xforwardfor = flag.Bool("f", true, "randomized X-Forwarded-For and X-Real-IP address")
var TerminalWriter = goterminal.New(os.Stdout)
var customIP ipArray

func main() {
	flag.Var(&customIP, "i", "custom ip address for that domain, multiple addresses automatically will be assigned randomly")
	flag.Parse()
	routines := *count

	if customIP != nil && len(customIP) > 0 && routines < len(customIP){
		routines = len(customIP)
	}

	go showStat()
	var waitgroup sync.WaitGroup
	if routines <= 0 {
		routines = 16
	}
	for i := 0; i < routines; i++ {
		waitgroup.Add(1)
		go goFun(*url, *postContent, *referer, *xforwardfor, customIP, &waitgroup)
	}
	waitgroup.Wait()
	TerminalWriter.Reset()
}
