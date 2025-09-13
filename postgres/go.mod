module github.com/go-estoria/estoria-examples/postgres

go 1.25.0

replace github.com/go-estoria/estoria => ../../estoria

replace github.com/go-estoria/estoria-contrib => ../../estoria-contrib

require (
	github.com/go-estoria/estoria v0.1.6
	github.com/go-estoria/estoria-contrib v0.0.0-20250128045749-70977af74f46
	github.com/gofrs/uuid/v5 v5.3.2
)

require github.com/lib/pq v1.10.9
