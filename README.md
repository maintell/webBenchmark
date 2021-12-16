# webBenchmark
http benchmark tool to ran out your server bandwidth.
- random User-Agent on every Request
- customizable Referer Url,
- concurrent routines as you wish, depends on you server performance.
- add http post mode
- specify target ip, or resolved by system dns.
- randomly X-Forwarded-For and X-Real-IP (default on).

# Todo 
- automatically tune concurrent routines to gain maximum performance. 
- randomly target ip.
- support NOT standard port in address with specify target ip.

# Usage: 
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          concurrent routines for download (default 16)
    -r string
          referer url
    -s string
          target url (default "https://baidu.com")
    -i string
          customize ip
    -f string
          randomized X-Forwarded-For and X-Real-IP address
    -p string
          post content


# LINUX:
    wget https://github.com/maintell/webBenchmark/releases/download/0.3/webBenchmark_linux_x64
    chmod +x webBenchmark_linux_x64
    ./webBenchmark_linux_x64 -c 32 -s https://target.url

## advanced example
    # send request to 1.1.1.1 for https://target.url with 32 concurrent threads 
    # and refer is https://refer.url
    ./webBenchmark_linux_x64 -c 32 -s https://target.url -r https://refer.url -i 1.1.1.1

    

