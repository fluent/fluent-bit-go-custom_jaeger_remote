all:
	go build -trimpath -buildmode=c-shared -o custom_jaeger_remote.so .

fast:
	go build custom_jaeger_remote.go

clean:
	rm -rf *.so *.h *~
