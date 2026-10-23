package ops

import "strings"

// messages 是操作者能看到的错误：键 → {中文, 英文}。{v} 换成 Error.Arg。
// API 用中文；网页后台按界面语言选。
var messages = map[string][2]string{
	"password.length":        {"口令要 12 到 72 个字节", "Passwords must be 12 to 72 bytes long."},
	"user.password_or_hash":  {"password 和 hash 只能给一个", "Give either a password or a hash, not both."},
	"user.hash":              {"hash 只接受 bcrypt", "Only bcrypt hashes are accepted."},
	"user.name":              {"用户名只能用字母、数字和 . _ -，以字母或数字开头，最长 64 个字符", "Names may contain letters, digits, dots, underscores and hyphens, start with a letter or digit, and be at most 64 characters long."},
	"user.in_file":           {"配置文件的 users_file 里已有这个用户", "This user is already in the users_file of the configuration."},
	"user.password_required": {"要给口令", "A password is required."},
	"user.exists":            {"用户已存在", "This user already exists."},
	"user.not_found":         {"没有这个用户（users_file 里的用户要改文件）", "No such user. Users from users_file are changed in that file."},
	"limit.manifest_rate":    {"每秒 manifest 请求数应在 0 到 1000 之间", "Manifest requests per second must be between 0 and 1000."},
	"limit.blob_streams":     {"同时下载的镜像层数应在 0 到 10000 之间", "Concurrent layer downloads must be between 0 and 10000."},
	"note.length":            {"备注最长 500 个字节", "Notes are at most 500 bytes long."},
	"cidr.invalid":           {"应写成 IP 或 CIDR，例如 198.51.100.7、198.51.100.0/24", "Write an IP address or a CIDR range, such as 198.51.100.7 or 198.51.100.0/24."},
	"cidr.all":               {"不能整段放开或封掉所有地址", "A rule cannot cover every address."},
	"kind.invalid":           {"kind 只能是 allow 或 deny", "The kind must be allow or deny."},
	"rule.exists":            {"规则已存在", "This rule already exists."},
	"rule.not_found":         {"没有这条规则（配置文件里的要改配置文件）", "No such rule. Rules from the configuration file are changed there."},
	"ban.duration":           {"期限应写成 30m、2h、7d 这样，1 分钟到 365 天", "Write the duration like 30m, 2h or 7d, from 1 minute to 365 days."},
	"ban.reason":             {"原因最长 500 个字节", "The reason is at most 500 bytes long."},
	"ban.not_found":          {"没有这个封禁", "No such ban."},
	"image.pattern":          {"规则写法不对：要写全上游，例如 ghcr.io/owner/**；* 匹配一段里的任意字符，** 匹配任意多段", "Invalid pattern. Start with the upstream, such as ghcr.io/owner/**; * matches within one segment and ** matches any number of segments."},
	"image.upstream":         {"第一段要写已配置的上游（或 *），例如 docker.io/library/nginx、ghcr.io/owner/**", "The first segment must be a configured upstream (or *), such as docker.io/library/nginx or ghcr.io/owner/**."},
	"image.status":           {"状态码只能是 403 或 451", "The status must be 403 or 451."},
	"image.text":             {"说明最长 300 个字节，备注最长 500 个字节", "The message is at most 300 bytes and the note at most 500 bytes long."},
	"upstream.not_found":     {"没有这个上游", "No such upstream."},
	"host.invalid":           {"要写一个完整的主机名，例如 cdn99.quay.io（不支持通配，通配请改配置文件）", "Write one full host name, such as cdn99.quay.io. Wildcards belong in the configuration file."},
	"host.exists":            {"已经在白名单里", "This host is already allowed."},
	"host.not_found":         {"白名单里没有这个主机（配置文件里的要改配置文件）", "This host is not in the list. Hosts from the configuration file are changed there."},
	"role.invalid":           {"角色只能是 admin 或 viewer", "The role must be admin or viewer."},
	"token.in_config":        {"配置文件里已有同名令牌", "The configuration file already has a token with this name."},
	"token.exists":           {"令牌已存在", "This token already exists."},
	"token.not_found":        {"没有这个令牌（配置文件里的要改配置文件）", "No such token. Tokens from the configuration file are changed there."},
	"admin.exists":           {"管理员已存在", "This administrator already exists."},
	"admin.self":             {"不能停用自己，也不能降低自己的角色", "You cannot disable yourself or lower your own role."},
	"admin.delete_self":      {"不能删除自己", "You cannot delete yourself."},
	"admin.last":             {"至少要留一个能用的 admin 角色", "At least one enabled administrator with the admin role must remain."},
	"admin.not_found":        {"没有这个管理员", "No such administrator."},
	"site.name":              {"站点名称必填，最长 80 个字节；运营者最长 120 个字节", "The site name is required and at most 80 bytes long; the operator is at most 120 bytes long."},
	"site.link_key":          {"不认识的链接 {v}", "Unknown link {v}."},
	"site.link":              {"链接 {v} 要写完整的 http(s) 地址", "The {v} link must be a full http(s) address."},
	"store.readonly":         {"运营数据是只读的（admin.store.driver: none），请改配置文件", "Operational data is read-only (admin.store.driver: none). Change the configuration file instead."},
	"reload.failed":          {"重新加载配置失败", "Reloading the configuration failed"},
}

// Message 返回这个错误在 lang（zh 或 en）下的说明。
func (e *Error) Message(lang string) string {
	m, ok := messages[e.Key]
	if !ok {
		return e.Key
	}
	text := m[0]
	if lang == "en" {
		text = m[1]
	}
	text = strings.ReplaceAll(text, "{v}", e.Arg)
	if e.Detail != "" {
		if lang == "en" {
			text += ": " + e.Detail
		} else {
			text += "：" + e.Detail
		}
	}
	return text
}
