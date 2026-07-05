package promptfilter

import "regexp"

var defaultPatternConfigs = []PatternConfig{
	{Name: "credential_theft", Pattern: `(?i)(?:^|[.!?。！？]\s*)(steal|dump|extract|exfiltrate|harvest|grab)\b.{0,50}\b(?:credentials?|passwords?|tokens?|cookies?)\b|\b(write|generate|create|give|build|craft|make|show|provide|implement|code|script|tool|steps?|instructions?|how\s+to|how\s+(?:can|do)\s+i|help\s+me|i\s+want\s+to|please|can\s+you)\b.{0,100}\b(steal|dump|extract|exfiltrate|harvest|grab)\b.{0,50}\b(?:credentials?|passwords?|tokens?|cookies?)\b|(?:写|生成|给我|构造|制作|提供|实现).{0,50}(窃取|导出|转储|提取).{0,30}(凭证|密码|令牌|token|cookie)`, Weight: 100, Category: "malicious", Strict: true},
	{Name: "malware_family", Pattern: `(?i)\b(keylogger|ransomware|trojan|backdoor|botnet|infostealer)\b`, Weight: 80, Category: "malware", Strict: true},
	{Name: "evasion", Pattern: `(?i)\b(bypass|disable|evade)\s+(av|edr|defender|antivirus|endpoint\s+detection)\b|免杀|绕过\s*(杀软|edr)`, Weight: 80, Category: "evasion", Strict: true},
	{Name: "persistence", Pattern: `(?i)\b(persistence|persist(?:ent)?\s+access|startup\s+persistence|registry\s+run\s+key)\b`, Weight: 35, Category: "post_exploitation"},
	{Name: "remote_shell", Pattern: `(?i)\b(reverse\s+shell|bind\s+shell|meterpreter|web\s+shell)\b|反弹\s*shell`, Weight: 45, Category: "remote_access"},
	{Name: "operational_remote_access_request", Pattern: `(?i)\b(write|generate|create|give|build|craft|make)\b.{0,80}\b(reverse\s+shell|bind\s+shell|meterpreter|web\s+shell)\b|(?:写|生成|给我|构造|制作).{0,40}反弹\s*shell`, Weight: 20, Category: "remote_access"},
	{Name: "exploit_payload", Pattern: `(?i)\b(exploit\s+payload|payload\s+for\s+exploiting|weaponiz(?:e|ed|ation))\b`, Weight: 45, Category: "exploit"},
	{Name: "operational_exploit_request", Pattern: `(?i)\b(write|generate|create|give|build|craft|make)\b.{0,80}\b(exploit|payload|poc|proof[-\s]?of[-\s]?concept|0day|zero[-\s]?day)\b|(?:写|生成|给我|构造|制作).{0,40}(漏洞利用|攻击载荷|payload|poc)`, Weight: 45, Category: "exploit"},
	{Name: "exploit_technique", Pattern: `(?i)\b(shellcode|rop\s+chain|heap\s+spray|buffer\s+overflow\s+exploit)\b`, Weight: 35, Category: "exploit"},
	{Name: "privilege_escalation", Pattern: `(?i)\b(privilege\s+escalation|privesc|root\s+exploit|local\s+root)\b|提权`, Weight: 35, Category: "post_exploitation"},
	{Name: "pentest_tooling", Pattern: `(?i)\b(metasploit|cobalt\s+strike|mimikatz|empire|sliver\s+c2)\b`, Weight: 30, Category: "tooling"},
	{Name: "scanner_tooling", Pattern: `(?i)\b(sqlmap|nmap|masscan|zmap|burp\s+suite)\b`, Weight: 15, Category: "tooling"},
	{Name: "large_scale_scanning", Pattern: `(?i)\b(large[-\s]?scale|internet[-\s]?wide|public\s+ip\s+ranges?|mass)\s+(scan|scanning|enumeration)\b`, Weight: 40, Category: "scanning"},
	{Name: "cve_reference", Pattern: `(?i)\bcve-\d{4}-\d{4,7}\b`, Weight: 10, Category: "vulnerability"},
	{Name: "generic_exploit", Pattern: `(?i)\b(exploit|payload|vulnerability|0day|zero[-\s]?day)\b`, Weight: 10, Category: "vulnerability"},
	{Name: "reverse_engineering", Pattern: `(?i)\b(ida\s+pro|ghidra|x64dbg|ollydbg|frida\s+hook|deobfuscate|unpack)\b|反编译|脱壳`, Weight: 15, Category: "reverse_engineering"},
	{Name: "reverse_engineering_secret_extraction", Pattern: `(?i)\b(ida\s+pro|ghidra|x64dbg|ollydbg|frida|jadx|apktool|decompile|disassembl|reverse\s+engineer)\b.{0,120}\b(extract|dump|recover|decrypt)\b.{0,80}\b(api\s*keys?|tokens?|secrets?|private\s*keys?|certificates?|license\s*keys?)\b|(?:ida|ghidra|frida|jadx|apktool|反编译|逆向).{0,80}(提取|导出|解密|恢复).{0,40}(密钥|token|令牌|私钥|证书|授权码)`, Weight: 90, Category: "reverse_engineering", Strict: true},
	{Name: "reverse_engineering_license_bypass", Pattern: `(?i)\b(ida\s+pro|ghidra|x64dbg|ollydbg|frida|jadx|apktool|decompile|disassembl|reverse\s+engineer)\b.{0,120}\b(bypass|crack|patch|remove|unlock)\b.{0,80}\b(license|activation|trial|paywall|subscription|in[-\s]?app\s+purchase|iap|entitlement)\b|(?:ida|ghidra|x64dbg|frida|反编译|逆向|脱壳|调试).{0,80}(绕过|破解|补丁|去除|解锁).{0,40}(授权|激活|试用|会员|订阅|付费|内购)`, Weight: 85, Category: "license_cracking", Strict: true},
	{Name: "reverse_engineering_anti_debug_bypass", Pattern: `(?i)\b(bypass|disable|remove|defeat)\b.{0,60}\b(anti[-\s]?debug|anti[-\s]?tamper|integrity\s+check|root\s+detection|jailbreak\s+detection|certificate\s+pinning)\b|绕过.{0,40}(反调试|反篡改|完整性校验|root\s*检测|越狱检测|证书绑定|证书固定)`, Weight: 70, Category: "reverse_engineering", Strict: true},
	{Name: "frida_hook_abuse", Pattern: `(?i)\b(frida|substrate|xposed)\b.{0,100}\b(hook|patch|bypass|unlock)\b.{0,80}\b(payment|purchase|license|activation|subscription|login|auth|entitlement)\b|(?:frida|xposed).{0,80}(hook|绕过|破解|解锁).{0,40}(支付|内购|授权|激活|会员|订阅|登录|鉴权)`, Weight: 75, Category: "reverse_engineering", Strict: true},
	{Name: "license_cracking", Pattern: `(?i)\b(keygen|crack\s+license|serial\s+generator|license\s+bypass|patch\s+(activation|license))\b|注册机|破解授权|序列号生成`, Weight: 55, Category: "license_cracking", Strict: true},
	{Name: "data_exfiltration", Pattern: `(?i)\b(exfiltrate|exfiltration|data\s+theft|steal\s+data|siphon\s+data)\b.{0,80}\b(database|files?|documents?|source\s+code|intellectual\s+property)\b|数据窃取|数据外泄`, Weight: 70, Category: "data_theft", Strict: true},
	{Name: "ddos_attack", Pattern: `(?i)\b(ddos|dos\s+attack|distributed\s+denial|amplification\s+attack|syn\s+flood|udp\s+flood)\b|拒绝服务攻击|流量攻击`, Weight: 65, Category: "network_attack", Strict: true},
	{Name: "cryptomining_hijack", Pattern: `(?i)\b(cryptojacking|coinhive|monero\s+miner|unauthorized\s+mining|hijack.{0,40}mining)\b|挖矿劫持|非法挖矿`, Weight: 60, Category: "resource_abuse", Strict: true},
	{Name: "phishing_social_engineering", Pattern: `(?i)\b(phishing\s+(page|site|email)|credential\s+harvesting|fake\s+login|spoof\s+(domain|website))\b|钓鱼页面|伪造登录`, Weight: 75, Category: "social_engineering", Strict: true},
	{Name: "supply_chain_attack", Pattern: `(?i)\b(supply\s+chain\s+attack|dependency\s+confusion|typosquatting|malicious\s+package|backdoor.{0,40}(npm|pypi|gem))\b|供应链攻击|依赖投毒`, Weight: 70, Category: "supply_chain", Strict: true},
	{Name: "container_escape", Pattern: `(?i)\b(container\s+escape|docker\s+breakout|kubernetes\s+escape|privileged\s+container\s+exploit)\b|容器逃逸`, Weight: 50, Category: "container_security"},
	{Name: "cloud_abuse", Pattern: `(?i)\b(aws\s+key\s+leak|gcp\s+credential|azure\s+token|s3\s+bucket\s+takeover|iam\s+privilege\s+escalation)\b|云凭证泄露`, Weight: 55, Category: "cloud_security"},
	{Name: "sql_injection_attack", Pattern: `(?i)\b(sql\s+injection\s+payload|union\s+select\s+attack|blind\s+sqli|time[-\s]?based\s+sqli)\b|sql注入攻击`, Weight: 40, Category: "web_attack"},
	{Name: "command_injection", Pattern: `(?i)\b(command\s+injection|os\s+command\s+injection|shell\s+injection|rce\s+exploit)\b|命令注入`, Weight: 50, Category: "web_attack"},
	{Name: "ssrf_xxe_attack", Pattern: `(?i)\b(ssrf\s+exploit|server[-\s]?side\s+request\s+forgery|xxe\s+attack|xml\s+external\s+entity)\b`, Weight: 35, Category: "web_attack"},
	{Name: "password_cracking", Pattern: `(?i)\b(hashcat|john\s+the\s+ripper|password\s+cracking|brute[-\s]?force\s+(password|hash)|rainbow\s+table)\b|密码破解|暴力破解`, Weight: 30, Category: "credential_attack"},
	{Name: "mitm_attack", Pattern: `(?i)\b(man[-\s]?in[-\s]?the[-\s]?middle|mitm\s+attack|arp\s+spoofing|dns\s+spoofing|ssl\s+strip)\b|中间人攻击`, Weight: 45, Category: "network_attack"},
	{Name: "wireless_attack", Pattern: `(?i)\b(wpa2?\s+crack|wifi\s+deauth|evil\s+twin|rogue\s+access\s+point|aircrack)\b|wifi破解|无线攻击`, Weight: 35, Category: "wireless_attack"},
	{Name: "firmware_iot_exploit", Pattern: `(?i)\b(firmware\s+extraction|iot\s+exploit|router\s+backdoor|embedded\s+device\s+hack)\b|固件提取|物联网攻击`, Weight: 40, Category: "iot_security"},
	{Name: "blockchain_exploit", Pattern: `(?i)\b(smart\s+contract\s+exploit|reentrancy\s+attack|flash\s+loan\s+attack|private\s+key\s+theft)\b|智能合约漏洞|私钥窃取`, Weight: 45, Category: "blockchain_security"},
	{Name: "session_hijacking", Pattern: `(?i)\b(session\s+hijacking|cookie\s+theft|session\s+fixation|csrf\s+exploit)\b|会话劫持|cookie窃取`, Weight: 40, Category: "web_attack"},
	{Name: "api_abuse", Pattern: `(?i)\b(api\s+key\s+leak|rate\s+limit\s+bypass|api\s+abuse|unauthorized\s+api\s+access)\b|api密钥泄露|接口滥用`, Weight: 35, Category: "api_security"},
	{Name: "steganography_covert", Pattern: `(?i)\b(steganography|covert\s+channel|data\s+hiding|exfiltration\s+via\s+(dns|icmp))\b|隐写术|隐蔽信道`, Weight: 30, Category: "evasion"},
	{Name: "ransomware_deployment", Pattern: `(?i)\b(deploy\s+ransomware|ransomware\s+payload|encrypt\s+files\s+for\s+ransom|wannacry|locky)\b|部署勒索软件|加密勒索`, Weight: 90, Category: "malware", Strict: true},
	{Name: "botnet_c2", Pattern: `(?i)\b(botnet\s+command|c2\s+server|command\s+and\s+control|zombie\s+network)\b|僵尸网络|c2服务器`, Weight: 65, Category: "malware", Strict: true},
	{Name: "xss_attack", Pattern: `(?i)\b(xss\s+payload|cross[-\s]?site\s+scripting\s+attack|stored\s+xss|reflected\s+xss|dom\s+xss)\b|xss攻击载荷`, Weight: 35, Category: "web_attack"},
	{Name: "deserialization_exploit", Pattern: `(?i)\b(deserialization\s+exploit|insecure\s+deserialization|java\s+deserialization\s+attack|pickle\s+exploit)\b|反序列化漏洞`, Weight: 45, Category: "web_attack"},
	{Name: "path_traversal", Pattern: `(?i)\b(path\s+traversal|directory\s+traversal|\.\.\/|lfi\s+exploit|local\s+file\s+inclusion)\b|目录遍历|文件包含`, Weight: 35, Category: "web_attack"},
	{Name: "memory_corruption", Pattern: `(?i)\b(use[-\s]?after[-\s]?free|double\s+free|heap\s+overflow|stack\s+overflow\s+exploit)\b|内存破坏|堆溢出`, Weight: 50, Category: "exploit"},
	{Name: "kernel_exploit", Pattern: `(?i)\b(kernel\s+exploit|kernel\s+module\s+rootkit|dirty\s+cow|privilege\s+escalation\s+via\s+kernel)\b|内核漏洞|内核提权`, Weight: 60, Category: "exploit", Strict: true},
	{Name: "zero_click_exploit", Pattern: `(?i)\b(zero[-\s]?click\s+exploit|remote\s+code\s+execution\s+without\s+interaction|wormable\s+exploit)\b|零点击漏洞`, Weight: 70, Category: "exploit", Strict: true},
	{Name: "sandbox_escape", Pattern: `(?i)\b(sandbox\s+escape|vm\s+escape|browser\s+sandbox\s+bypass|jvm\s+sandbox\s+escape)\b|沙箱逃逸|虚拟机逃逸`, Weight: 55, Category: "exploit"},
	{Name: "firmware_backdoor", Pattern: `(?i)\b(firmware\s+backdoor|bios\s+rootkit|uefi\s+malware|bootkit)\b|固件后门|bios木马`, Weight: 75, Category: "malware", Strict: true},
	{Name: "supply_chain_backdoor", Pattern: `(?i)\b(backdoor.{0,40}(npm|pypi|rubygems|maven)|trojanized\s+package|malicious\s+dependency)\b|依赖后门|恶意包`, Weight: 70, Category: "supply_chain", Strict: true},
	{Name: "credential_dumping", Pattern: `(?i)\b(lsass\s+dump|sam\s+dump|ntds\.dit|credential\s+dumping|hashdump)\b|凭证转储|密码哈希导出`, Weight: 65, Category: "credential_attack", Strict: true},
	{Name: "lateral_movement", Pattern: `(?i)\b(lateral\s+movement|pass[-\s]?the[-\s]?hash|pass[-\s]?the[-\s]?ticket|psexec|wmi\s+exec)\b|横向移动`, Weight: 50, Category: "post_exploitation"},
	{Name: "domain_takeover", Pattern: `(?i)\b(domain\s+takeover|subdomain\s+hijacking|dns\s+takeover|dangling\s+cname)\b|域名劫持|子域接管`, Weight: 55, Category: "network_attack"},
	{Name: "token_theft", Pattern: `(?i)\b(oauth\s+token\s+theft|jwt\s+hijacking|bearer\s+token\s+steal|access\s+token\s+exfiltration)\b|token窃取|令牌劫持`, Weight: 60, Category: "credential_attack", Strict: true},
	{Name: "process_injection", Pattern: `(?i)\b(process\s+injection|dll\s+injection|reflective\s+loading|process\s+hollowing)\b|进程注入|dll注入`, Weight: 55, Category: "evasion"},
	{Name: "fileless_malware", Pattern: `(?i)\b(fileless\s+malware|living\s+off\s+the\s+land|lolbins|powershell\s+empire)\b|无文件攻击`, Weight: 50, Category: "evasion"},
	{Name: "log_tampering", Pattern: `(?i)\b(log\s+deletion|event\s+log\s+clearing|anti[-\s]?forensics|cover\s+tracks)\b|日志清除|反取证`, Weight: 45, Category: "evasion"},
	{Name: "vpn_proxy_abuse", Pattern: `(?i)\b(vpn\s+exploit|proxy\s+chain\s+for\s+anonymity|tor\s+hidden\s+service\s+setup)\b|vpn漏洞|代理链匿名`, Weight: 30, Category: "evasion"},
	{Name: "database_dump", Pattern: `(?i)\b(database\s+dump|mysqldump\s+attack|mongodb\s+ransom|elasticsearch\s+exposure)\b|数据库导出|数据库勒索`, Weight: 55, Category: "data_theft"},
	{Name: "api_key_scraping", Pattern: `(?i)\b(scrape\s+api\s+keys|github\s+secret\s+scanning|hardcoded\s+credentials\s+search)\b|api密钥爬取|硬编码凭证搜索`, Weight: 50, Category: "credential_attack"},
	{Name: "mass_exploitation", Pattern: `(?i)\b(mass\s+exploitation|automated\s+exploitation|exploit\s+at\s+scale|worm\s+propagation)\b|大规模利用|蠕虫传播`, Weight: 70, Category: "exploit", Strict: true},
	{Name: "insider_threat", Pattern: `(?i)\b(insider\s+threat|rogue\s+employee|data\s+theft\s+by\s+employee|sabotage)\b|内部威胁|员工窃密`, Weight: 40, Category: "data_theft"},
	{Name: "cryptographic_attack", Pattern: `(?i)\b(padding\s+oracle|timing\s+attack|side[-\s]?channel\s+attack|weak\s+encryption\s+exploit)\b|密码学攻击|侧信道攻击`, Weight: 35, Category: "crypto_attack"},
	{Name: "race_condition_exploit", Pattern: `(?i)\b(race\s+condition\s+exploit|toctou|time[-\s]?of[-\s]?check\s+time[-\s]?of[-\s]?use)\b|竞态条件漏洞`, Weight: 30, Category: "exploit"},
	{Name: "hardware_implant", Pattern: `(?i)\b(hardware\s+implant|usb\s+rubber\s+ducky|malicious\s+usb|hardware\s+keylogger)\b|硬件植入|恶意usb`, Weight: 60, Category: "physical_attack", Strict: true},
	{Name: "social_media_hijack", Pattern: `(?i)\b(account\s+takeover|social\s+media\s+hijacking|credential\s+stuffing)\b|账号接管|撞库攻击`, Weight: 40, Category: "credential_attack"},
	// --- operational authoring patterns (build-verb + malicious artifact): high signal, low false-positive ---
	{Name: "malware_authoring", Pattern: `(?i)\b(write|create|generate|build|develop|code|program|make)\b[^.!?\n]{0,50}\b(malware|virus|trojan|worm|ransomware|spyware|info[-\s]?stealer|stealer|rootkit|\brat\b|remote\s+access\s+trojan|bootkit|wiper)\b|(?:写|编写|生成|制作|开发)[^。！？\n]{0,30}(恶意软件|病毒|木马|蠕虫|勒索软件|间谍软件|窃密程序)`, Weight: 90, Category: "malware", Strict: true},
	{Name: "mfa_bypass", Pattern: `(?i)\b(bypass|defeat|circumvent|disable|get\s+around)\b[^.!?\n]{0,30}\b(2fa|mfa|two[-\s]?factor|multi[-\s]?factor|otp|one[-\s]?time\s+password|authenticator)\b|绕过[^。！？\n]{0,20}(二次验证|双因素|两步验证|otp|验证码)`, Weight: 70, Category: "credential_attack", Strict: true},
	{Name: "phishing_generation", Pattern: `(?i)\b(write|generate|create|draft|compose|craft)\b[^.!?\n]{0,40}\b(phishing|spear[-\s]?phishing|smishing|scam)\b[^.!?\n]{0,20}\b(email|e-mail|message|sms|text|page|template|lure)\b|(?:写|生成|制作|起草)[^。！？\n]{0,30}(钓鱼|诈骗)[^。！？\n]{0,15}(邮件|短信|页面|模板)`, Weight: 75, Category: "social_engineering", Strict: true},
	{Name: "fraud_carding", Pattern: `(?i)\b(carding|card\s+dumps?|cvv\s+dumps?|dumps?\s+(?:with|and)\s+pins?|fullz|bank\s+drops?|cashout\s+(?:method|guide))\b|信用卡盗刷|盗刷教程`, Weight: 70, Category: "fraud", Strict: true},
	{Name: "spam_automation", Pattern: `(?i)\b(mass|bulk|automated)\b[^.!?\n]{0,20}\b(spam|unsolicited\s+email|robocall|sms\s+blast)\b|群发[^。！？\n]{0,15}(垃圾邮件|短信|骚扰)`, Weight: 35, Category: "abuse"},
}


var sensitiveRedactionPatterns = []struct {
	re          *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*(?:bearer|basic)\s+)[^\s,;]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)(["']?\b(?:password|passwd|pwd|token|api[_-]?key|secret|client[_-]?secret|access[_-]?token|refresh[_-]?token|session[_-]?id)\b["']?\s*[:=]\s*["']?)[^"',\s}]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`(?i)\b(cookie\s*[:=]\s*)[^\n]+`), `${1}[REDACTED]`},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9][A-Za-z0-9_-]{7,}\b`), `[REDACTED_API_KEY]`},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), `[REDACTED_JWT]`},
	{regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`), `[REDACTED_EMAIL]`},
}

var defensiveContextPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(defensive|defense|prevent|prevention|mitigation|detect|detection|hardening|patch|remediation|incident\s+response)\b`),
	regexp.MustCompile(`(?i)\b(do\s+not\s+provide|without\s+code|no\s+commands|high\s+level|non[-\s]?operational|refusal|unsafe)\b`),
	regexp.MustCompile(`防御|修复|检测|加固|不要提供|不提供代码`),
}

// operationalArtifactPattern detects requests that ask the model to actually
// BUILD/emit a runnable offensive artifact (code/script/payload/tool/command).
// When present, the "defensive context" discount is suppressed: genuine defensive
// questions explain/describe indicators ("no code", "high level"), whereas an
// attacker's fig-leaf pairs defensive words with "write me a working keylogger".
// This closes the discount bypass without over-blocking real defensive discussion.
var operationalArtifactPattern = regexp.MustCompile(`(?i)\b(write|generate|create|build|craft|make|produce|develop|implement|compile|deploy|obfuscate|weaponize)\b[^.!?\n]{0,60}\b(code|script|program|tool|payload|exploit|malware|keylogger|ransomware|stealer|rootkit|shell|shellcode|binary|executable|command|one[-\s]?liner|snippet|implementation|poc|proof[-\s]?of[-\s]?concept)\b|\b(full|complete|working|functional|ready[-\s]?to[-\s]?(?:use|run)|copy[-\s]?paste)\b[^.!?\n]{0,40}\b(code|script|payload|exploit|tool|program)\b|(?:写|编写|生成|制作|实现|构造|给出|提供)[^。！？\n]{0,30}(代码|脚本|payload|载荷|程序|工具|命令|完整|可运行)`)

