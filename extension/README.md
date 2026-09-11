# 远程迅雷下载 Chrome 扩展

扩展使用 Chrome Manifest V3，通过右键菜单调用 `xunlei-api`。它不会读取网页 Cookie，也不会访问浏览历史；只有用户保存的远程 API 地址会获得主机访问权限。

## 安装

1. 从 GitHub Release 下载 `xunlei-api-chrome-extension_<版本>.zip` 并解压。
2. 打开 `chrome://extensions`，启用“开发者模式”。
3. 选择“加载已解压的扩展程序”，选中解压后的目录。
4. 在自动打开的设置页填写远程地址和 API Key，保存后点击“测试连接”。
5. 根据需要修改“电影”“音乐”“电子书”等类目的迅雷展示路径，再点击“验证所有路径”。

扩展固定 ID 为 `pjfbokcciledhphokcaookifodbgdmbb`。服务端若启用 CORS，应准确允许来源：

```text
chrome-extension://pjfbokcciledhphokcaookifodbgdmbb
```

默认、电影、音乐和电子书四个类目初始都指向 `/downloads`，确保首次安装即可使用。默认类目不能删除；其他类目可以新增、修改或删除。

## 右键下载

在网页空白处、链接、图片、音频、视频或选中的 HTTP、HTTPS、magnet 地址上点击右键，选择“添加到远程迅雷下载”，再选择类目。页面右上角会先显示正在添加，远程 API 返回后更新为成功或失败；浏览器禁止页面注入时改用系统通知。
