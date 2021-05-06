package main

import (
	"flag"
	"fmt"
	"github.com/EDDYCJY/fake-useragent"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/apoorvam/goterminal"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/load"
	"github.com/shirou/gopsutil/mem"
	"github.com/shirou/gopsutil/net"
)

const (
	letterIdxBits = 6
	letterIdxMask = 1<<letterIdxBits - 1
	letterIdxMax  = 63 / letterIdxBits
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

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

func showStat() {
	initialNetCounter, _ := net.IOCounters(true)
	for true {
		percent, _ := cpu.Percent(time.Second, false)
		memStat, _ := mem.VirtualMemory()
		netCounter, _ := net.IOCounters(true)
		loadStat, _ := load.Avg()

		fmt.Fprintf(TerminalWriter, "cpu percent:%.3f%% \n", percent)
		fmt.Fprintf(TerminalWriter, "mem percent:%v%% \n", memStat.UsedPercent)
		fmt.Fprintf(TerminalWriter, "load info:%.3f %.3f %.3f\n", loadStat.Load1, loadStat.Load5, loadStat.Load15)
		for i := 0; i < len(netCounter); i++ {
			if netCounter[i].BytesRecv == 0 && netCounter[i].BytesSent == 0 {
				continue
			}
			fmt.Fprintf(TerminalWriter, "Nic:%v %s/s %s/s\n", netCounter[i].Name,
				readableBytes(float64(netCounter[i].BytesRecv-initialNetCounter[i].BytesRecv)),
				readableBytes(float64(netCounter[i].BytesSent-initialNetCounter[i].BytesSent)))
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

func goFun(Url string, Referer string, XforwardFor bool, wg *sync.WaitGroup) {
	for true {
		client := &http.Client{}
		request, err1 := http.NewRequest("GET", Url, nil)
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

var count = flag.Int("c", 16, "cocurrent thread for download")
var url = flag.String("s", "https://baidu.com", "target url")
var referer = flag.String("r", "", "referer url")
var xforwardfor = flag.Bool("f", false, "random X-Forwarded-For ip address")
var TerminalWriter = goterminal.New(os.Stdout)

func main() {
	flag.Parse()
	go showStat()
	var waitgroup sync.WaitGroup
	if *count <= 0 {
		*count = 16
	}
	for i := 0; i < *count; i++ {
		waitgroup.Add(1)
		go goFun(*url, *referer, *xforwardfor, &waitgroup)
	}
	waitgroup.Wait()
	TerminalWriter.Reset()
}
