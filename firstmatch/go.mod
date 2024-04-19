module open-match.dev/open-match-ecosystem/firstmatch

go 1.19

require (
	google.golang.org/grpc v1.53.0
	open-match.dev/open-match v1.7.0
	open-match.dev/open-match-ecosystem/demoui v0.0.0-local
)

require (
	github.com/golang/glog v1.0.0 // indirect
	github.com/golang/protobuf v1.5.2 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.15.1 // indirect
	golang.org/x/net v0.23.0 // indirect
	golang.org/x/sys v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto v0.0.0-20230222225845-10f96fb3dbec // indirect
	google.golang.org/protobuf v1.28.1 // indirect
)

replace open-match.dev/open-match-ecosystem/demoui v0.0.0-local => ../demoui
