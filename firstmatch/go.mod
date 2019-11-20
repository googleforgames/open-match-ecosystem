module open-match.dev/open-match-ecosystem/firstmatch

go 1.13

require (
	google.golang.org/grpc v1.25.0
	open-match.dev/open-match v0.8.0
	open-match.dev/open-match-ecosystem/demoui v0.0.0-local
)

replace open-match.dev/open-match-ecosystem/demoui v0.0.0-local => ../demoui
