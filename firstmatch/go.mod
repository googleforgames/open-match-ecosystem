module open-match.dev/open-match-ecosystem/firstmatch

go 1.24.0

require (
	google.golang.org/grpc v1.79.3
	open-match.dev/open-match v1.7.0
	open-match.dev/open-match-ecosystem/demoui v0.0.0-local
)

require (
	github.com/golang/glog v1.2.5 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.15.1 // indirect
	golang.org/x/net v0.48.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
	golang.org/x/text v0.32.0 // indirect
	google.golang.org/genproto v0.0.0-20230222225845-10f96fb3dbec // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

replace open-match.dev/open-match-ecosystem/demoui v0.0.0-local => ../demoui
