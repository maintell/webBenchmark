# webBenchmark
http benchmark tool to ran out your server bandwidth.
- random User-Agent on every Request
- random x-forward-for and x-Real-ip on every Request
- customizable Referer Url
- cocurrent thread as you wish, depends on you server performance.
- post method added.

# Usage: 
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          cocurrent thread for download (default 8)
    -r string
          referer url
    -f
          random x-forward-for and x-Real-ip header.
    -p string
          post content, request will be get if missing. otherwise post
    -s string
          target url (default "https://baidu.com")

# LINUX:
    wget https://github.com/maintell/webBenchmark/releases/download/0.2/webBenchmark_linux_x64 -o webBenchmark_linux_x64
    chmod +x webBenchmark_linux_x64
    ./webBenchmark_linux_x64 -c 32 -s https://target.url

