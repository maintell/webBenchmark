@echo off
SET CGO_ENABLED=0
SET GOOS=linux
SET GOARCH=amd64
go build -o webBenchmark_linux_x64 -buildvcs=false
SET GOOS=linux
SET GOARCH=arm
go build -o webBenchmark_linux_arm -buildvcs=false
SET GOOS=windows
SET GOARCH=amd64
go build -o webBenchmark_x64.exe -buildvcs=false
SET GOOS=freebsd
SET GOARCH=amd64
go build -o webBenchmark_bsd_x64.exe -buildvcs=false