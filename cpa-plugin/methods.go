package main

// 官方 sdk/pluginabi 里的方法名常量。
//
// 这里刻意**不** import github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi：
// 那个模块会带进整棵依赖树，而插件只需要方法名和 3 个字段的信封结构。
// 自己声明可以让插件保持零第三方依赖（除 YAML 解析外），构建更快、更好分发。
const (
	methodPluginRegister    = "plugin.register"
	methodPluginQuiesce     = "plugin.quiesce"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginShutdown    = "plugin.shutdown"

	methodModelRegister = "model.register"
	methodModelStatic   = "model.static"
	methodModelForAuth  = "model.for_auth"

	methodAuthIdentifier = "auth.identifier"
	methodAuthParse      = "auth.parse"
	methodAuthLoginStart = "auth.login.start"
	methodAuthLoginPoll  = "auth.login.poll"
	methodAuthRefresh    = "auth.refresh"

	methodExecutorIdentifier    = "executor.identifier"
	methodExecutorExecute       = "executor.execute"
	methodExecutorExecuteStream = "executor.execute_stream"
	methodExecutorCountTokens   = "executor.count_tokens"

	methodRequestNormalize = "request.normalize"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"

	methodHostHTTPDo          = "host.http.do"
	methodHostHTTPDoStream    = "host.http.do_stream"
	methodHostHTTPStreamRead  = "host.http.stream_read"
	methodHostHTTPStreamClose = "host.http.stream_close"
	methodHostStreamEmit      = "host.stream.emit"
	methodHostStreamClose     = "host.stream.close"
	methodHostLog             = "host.log"
	methodHostAuthList        = "host.auth.list"
)
