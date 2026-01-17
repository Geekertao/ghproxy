# ghproxy

这是 [CF-Workers-Github-Proxy](https://github.com/Geekertao/CF-Workers-GitHub-Proxy) 的 Go 语言版本。改自[gh-proxy](https://github.com/oopsunix/ghproxy)

## 支持内容

• GitHub API (api.github.com)

• Raw 文件 (raw.githubusercontent.com)

• Gist 文件 (gist.githubusercontent.com)

• Releases 文件 (github.com/用户名/仓库名/releases|archive/)

• Git 克隆支持 (git clone)


## Docker 部署

1. 新建配置文件 `config.json`，内容如下：
```json
{
  "analyticsURL": "https://analytics.geekertao.top",
  "supportImageURL": "https://geekertao.top/static/img/zsm.jpg",
  "whiteList": [
  ],
  "blackList": [
    "example1",
    "example2"
  ]
}
```
> 配置文件中的 [`analyticsURL`](#数据分析)是顶部导航栏"数据分析"按钮的链接地址，`supportImageURL` 是404错误页面中支持二维码的图片地址，`whiteList` 和 `blackList` 分别是白名单和黑名单，如果设置白名单。则只有白名单中的域名会被代理，黑名单可用于过滤部分不安全仓库。

2. docker-compose.yml
```yaml
services:
  ghproxy:
    image: Geekertao/ghproxy:latest # Docker Hub 镜像源
    # image：ghcr.io/Geekertao/ghproxy:latest # GitHub 镜像源
    container_name: ghproxy
    restart: unless-stopped
    volumes:
      - ./config.json:/app/config.json # 配置文件挂载路径，根据实际情况修改
    ports:
      - "45001:45000" # 端口映射，根据实际情况修改
```

3. 启动容器
```bash
docker-compose up -d
```

## 数据分析

本项目的数据分析功能由 [cloudflare-monitor](https://github.com/Geekertao/cloudflare-monitor) 提供，这是一个功能强大的 Cloudflare 流量分析仪表盘。

### 功能特性

- **多账户支持**：支持多个 Cloudflare 账户的流量监控
- **多 Zone 监控**：同时监控多个 Zone 的流量数据
- **实时数据展示**：实时更新的数据图表
- **历史数据分析**：支持 1 天、3 天、7 天、30 天的历史数据查看
- **智能数据精度**：
  - 1 天和 3 天数据：小时级精度
  - 7 天和 30 天数据：天级精度
- **地理位置统计**：显示前 5 个国家/地区的访问量
- **缓存分析**：详细的请求和带宽缓存统计
- **响应式设计**：完美适配桌面端和移动端

### 技术栈

- 前端：React + Recharts
- 后端：Node.js + Express
- 部署：Docker + Nginx

### 在线演示

[https://analytics.geekertao.top](https://analytics.geekertao.top)

### 自定义配置

如需使用自己的数据分析页面，请在 `config.json` 中修改 `analyticsURL` 配置项。

### 部署说明

详细的部署教程请参考：[cloudflare-monitor 仓库](https://github.com/Geekertao/cloudflare-monitor) 或 [博客文章教程](https://blog.geekertao.top/posts/cloudflare-analytics)（含视频部署教程）。


---

如有问题或改进建议，欢迎提交 [issue] 或 PR！


[issue]: https://github.com/Geekertao/ghproxy/issues