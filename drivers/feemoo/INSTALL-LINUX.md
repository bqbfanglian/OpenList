# Linux 安装：飞猫盘定制版

程序内置网页界面，不需要安装 Go 或 Docker。

1. 在 Linux 终端运行 `uname -m`。输出 `x86_64` 选 **linux-amd64**；输出 `aarch64` / `arm64` 选 **linux-arm64**。不支持 32 位 ARM 系统。
2. 将对应安装包传到 Linux，在可写目录解压。以 amd64 的本地交付包为例：

   ```sh
   tar -xzf openlist-feemoo-parallel-linux-amd64.tar.gz
   cd openlist-feemoo-parallel-linux-amd64
   sha256sum -c SHA256SUMS.txt
   chmod +x openlist start.sh
   ./start.sh
   ```

   ARM64 将命令中的 `amd64` 换成 `arm64`。GitHub Actions 的 Artifacts 下载为 ZIP，解压后进入包含 openlist 的目录，再运行后面三条命令。
3. 保持终端运行，在浏览器打开 `http://服务器IP:5244`，同机访问可用 `http://127.0.0.1:5244`。首次访问按初始化向导设置管理员。远程访问须允许你连接相应端口。
4. 进入 **后台 → 存储 → 添加 → Feemoo**，挂载路径填 `/feemoo`，根目录 ID 填 `0`。在桌面电脑打开包内“会话信息提取.html”，按照[完整说明](README.md)提取并填写自己的网页登录会话。不要向他人发送凭据。
5. `part_concurrency` 默认 `0`，按账号权限自动并行分片。临时目录留空或指定 Linux 上空间足够的可写磁盘目录。
6. 在来源网盘选择文件夹 → 复制 → 选择 `/feemoo` 下的目录 → 确认。查看“任务 → 复制”，并到飞猫盘官网核对结果。

start.sh 会切换到程序目录，默认在该目录生成 data。指定其他数据目录可用 `./start.sh --data /你的数据目录`。关闭前台程序按 Ctrl+C。

退出终端后继续运行，可先停止前台程序，再执行 `nohup ./start.sh > openlist.log 2>&1 &`。此方式不会自动配置开机启动；日志写入当前目录的 openlist.log。

## 升级已有安装

1. 停止原来的实例；如果使用 systemd 或其他服务管理器，按原来的方式停止。
2. 备份原程序、整个 data 目录，以及使用中的外部数据库。
3. 解压新包，仅将 openlist 替换到原来的程序位置，并执行 `chmod +x openlist`。
4. 保留原来的 data、config、工作目录、启动参数和服务配置，按原方式启动。不要让两个实例同时写同一个数据库。
5. 在后台编辑已有 Feemoo 存储，检查 `part_concurrency` 为 `0` 并保存。

Windows 的 openlist.exe 不能在 Linux 上运行。跨系统迁移时，本地挂载路径及临时目录也需改为 Linux 实际路径。

两个 Linux 架构由 GitHub Actions 编译，Linux amd64 Docker 包含 version 冒烟检查；实际结果以工作流为准。本次并行上传及原生跨存储复制已在开发用 Windows 环境真实验证，尚未在用户 Linux 环境实测。完整范围见 VALIDATION.md。
