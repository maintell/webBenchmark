# webBenchmark
http benchmark tool to ran out your server bandwidth.
- random User-Agent on every Request
- customizable Referer Url
- cocurrent thread as you wish, depends on you server performance.

# Usage: 
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          cocurrent thread for download (default 8)
    -r string
          referer url
    -s string
          target url (default "https://baidu.com")

# LINUX:
    wget https://github.com/maintell/webBenchmark/releases/download/0.1/webBenchmark_linux_x64
    chmod +x webBenchmark_linux_x64
    ./webBenchmark_linux_x64 -c 32 -s https://target.url

