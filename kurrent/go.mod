module github.com/go-estoria/estoria-examples/current

go 1.25.0

replace github.com/go-estoria/estoria => ../../estoria

replace github.com/go-estoria/estoria-contrib => ../../estoria-contrib

require (
	github.com/go-estoria/estoria v0.1.6
	github.com/go-estoria/estoria-contrib v0.0.0-20250128045749-70977af74f46
	github.com/gofrs/uuid/v5 v5.3.2
)

require (
	github.com/kurrent-io/KurrentDB-Client-Go v1.0.1
	github.com/lib/pq v1.10.9
)

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sys v0.31.0 // indirect
	golang.org/x/text v0.23.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250324211829-b45e905df463 // indirect
	google.golang.org/grpc v1.71.0 // indirect
	google.golang.org/protobuf v1.36.6 // indirect
)
