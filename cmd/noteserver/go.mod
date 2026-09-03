module noteserver

go 1.24.8

require (
	actor v0.0.0
	github.com/bytedance/sonic v1.15.3
	github.com/gin-gonic/gin v1.10.0
	github.com/norm v0.0.0
	github.com/sirupsen/logrus v1.10.2
	github.com/yinchnag/GCore v0.0.0-00010101000000-000000000000
	golang.org/x/crypto v0.40.0
	web v0.0.0-00010101000000-000000000000
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/bwmarrin/snowflake v0.3.0 // indirect
	github.com/bytedance/gopkg v0.1.3 // indirect
	github.com/bytedance/sonic/loader v0.5.2 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/cloudwego/base64x v0.1.6 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/gabriel-vasile/mimetype v1.4.3 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.20.0 // indirect
	github.com/go-redis/redis/v8 v8.11.5 // indirect
	github.com/go-sql-driver/mysql v1.9.3 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.3.0 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible // indirect
	github.com/lestrrat-go/strftime v1.1.1 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.2.2 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rifflock/lfshook v0.0.0-20180920164130-b9218ef580f5 // indirect
	github.com/timandy/routine v1.1.6 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.2.12 // indirect
	golang.org/x/arch v0.22.0 // indirect
	golang.org/x/net v0.42.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.27.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// actor 框架就在上两级目录，本示例随框架一起演进，不走版本发布。
replace actor => ../..

// 开发期指向本地检出。Norm 打上 tag 之后换成正式版本号并删掉这行。
replace github.com/norm => D:/cloud/Norm

// 日志库。同样是开发期指向本地检出，打 tag 后换成正式版本号。
replace github.com/yinchnag/GCore => D:/cloud/GCore

// 自动路由基座，与 actor 平级的另一个可复用件。它不依赖 actor，
// 单独成模块是为了让 gin 那一串传递依赖不进 actor 的 go.mod。
replace web => ../../web
