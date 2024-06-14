# updateBestCFIP

自动从 [https://ipdb.api.030101.xyz/?type=bestcf&country=true](https://ipdb.api.030101.xyz/?type=bestcf&country=true) 获取官方优选IP，取ping值最低的ip绑定域名。

使用方法
配置config.json

{"email": "账户email", "key": "账户key", "domainInfos":[["名称", "根域", "需要屏蔽地区的关键字"], ["名称", "根域"]]}

比如完整域名为 A.B.com，根域为 B.com 则config.json 为

{"email": "账户email", "key": "账户key", "domainInfos":[["A", "B.com"]]},

由于谷歌双子星等一些网站限制某些区域访问，若屏蔽香港区域，则 config.json 为

{"email": "账户email", "key": "账户key", "domainInfos":[["A", "B.com", "HK"]}

具体文件名参考 [https://zip.baipiao.eu.org](https://ipdb.api.030101.xyz)
