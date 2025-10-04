module github.com/go-estoria/estoria-examples/postgres

go 1.25.1

replace github.com/go-estoria/estoria => ../../estoria

replace github.com/go-estoria/estoria-contrib => ../../estoria-contrib

require (
	github.com/go-estoria/estoria v0.2.0
	github.com/go-estoria/estoria-contrib v0.0.0-20250128045749-70977af74f46
	github.com/gofrs/uuid/v5 v5.3.2
)

require github.com/lib/pq v1.10.9
