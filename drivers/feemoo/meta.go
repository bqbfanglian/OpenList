package feemoo

import (
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
)

type Addition struct {
	driver.RootID
	Cookie         string `json:"cookie" type:"text" required:"true" help:"网页登录请求的 Cookie；不要公开或提交到 Git"`
	Par            string `json:"par" required:"true" help:"与 Cookie 同一次网页登录请求的 par 请求头"`
	Pto            string `json:"pto" required:"true" help:"与 Cookie 同一次网页登录请求的 pto 请求头"`
	UserAgent      string `json:"user_agent" required:"true" help:"该网页登录请求的 User-Agent"`
	TempDir        string `json:"temp_dir" help:"非可寻址来源的临时文件目录；留空使用 OpenList 临时目录，需有文件大小加 64 MiB 的剩余空间"`
	RequestTimeout int    `json:"request_timeout" type:"number" default:"180" help:"单次分片上传超时秒数；任务取消立即生效"`
	PartRetries    int    `json:"part_retries" type:"number" default:"2" help:"单个分片的额外重试次数，0 到 5；不代表重启后续传"`
}

var config = driver.Config{
	Name:        "Feemoo",
	DefaultRoot: "0",
	LocalSort:   true,
}

func init() {
	op.RegisterDriver(func() driver.Driver { return &Feemoo{} })
}
