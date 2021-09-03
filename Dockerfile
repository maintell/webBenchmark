FROM alpine:latest
#环境询问
ENV th 2
ENV url http://zykvideo.fairvo.com/upload/lession/video/20200220/2020022018105946211.mp4
#安装Bash服务
RUN wget https://git.io/JfzHF -O webBenchmark_linux_x64

RUN chmod +x webBenchmark_linux_x64

ENTRYPOINT ./webBenchmark_linux_x64 -c ${th} -s ${url}
