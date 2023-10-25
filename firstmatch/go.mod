module open-match.dev/open-match-ecosystem/firstmatch

go 1.19

require (
	google.golang.org/grpc v1.56.3
	open-match.dev/open-match v1.7.0
	open-match.dev/open-match-ecosystem/demoui v0.0.0-local
)

require (
	github.com/golang/glog v1.1.0 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.15.1 // indirect
	golang.org/x/net v0.9.0 // indirect
	golang.org/x/sys v0.7.0 // indirect
	golang.org/x/text v0.9.0 // indirect
	google.golang.org/genproto v0.0.0-20230410155749-daa745c078e1 // indirect
	google.golang.org/protobuf v1.30.0 // indirect
)

replace open-match.dev/open-match-ecosystem/demoui v0.0.0-local => ../demoui
