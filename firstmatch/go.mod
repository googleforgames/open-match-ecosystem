module open-match.dev/open-match-ecosystem/firstmatch

go 1.13

require (
	google.golang.org/grpc v1.24.0
	open-match.dev/open-match v0.4.1-0.20191028202034-3899bd2fcdce
	open-match.dev/open-match-ecosystem/demoui v0.0.0-00010101000000-000000000000
)

replace open-match.dev/open-match-ecosystem/demoui => ../demoui
