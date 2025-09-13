module github.com/go-estoria/estoria-examples/mongodb

go 1.25.0

replace github.com/go-estoria/estoria => ../../estoria

replace github.com/go-estoria/estoria-contrib => ../../estoria-contrib

require (
	github.com/go-estoria/estoria v0.1.6
	github.com/go-estoria/estoria-contrib v0.0.0-20250128045749-70977af74f46
	github.com/gofrs/uuid/v5 v5.3.2
)

require go.mongodb.org/mongo-driver/v2 v2.0.0

require (
	github.com/golang/snappy v0.0.4 // indirect
	github.com/klauspost/compress v1.17.4 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)
