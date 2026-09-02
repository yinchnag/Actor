module actor

go 1.24.8

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/stretchr/testify v1.12.1 // indirect
)

require (
	github.com/go-sql-driver/mysql v1.9.3
	github.com/joho/godotenv v1.5.1
	github.com/redis/go-redis/v9 v9.7.0
	github.com/yinchnag/GCore v0.0.0-00010101000000-000000000000
	go.uber.org/goleak v1.3.0
	golang.org/x/crypto v0.33.0
)

// 开发期指向本地检出。GCore 打上 tag 之后换成正式版本号并删掉这行。
replace github.com/yinchnag/GCore => D:/cloud/GCore
