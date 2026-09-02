package protocol

// MessageRole 表示消息在通信中的固定语义角色。
//
// 平台能力依赖固定语义，因此预定义且封闭，不允许用户随便扩展定义：
//   - request  客户端发起的请求
//   - response 服务端对请求的响应
//   - push     服务端主动下发的通知（无请求对应）
//   - unknown  未能识别到任何角色
type MessageRole string

// 预定义的通信角色。
const (
	RoleRequest  MessageRole = "request"
	RoleResponse MessageRole = "response"
	RolePush     MessageRole = "push"
	RoleUnknown  MessageRole = "unknown"
)

// ValidRole 判断字符串是否是一个合法的预定义角色。
func ValidRole(r string) bool {
	switch MessageRole(r) {
	case RoleRequest, RoleResponse, RolePush, RoleUnknown:
		return true
	default:
		return false
	}
}
