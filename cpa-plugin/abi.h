/*
 * CLIProxyAPI 标准动态库插件 C ABI。
 *
 * 这份声明与官方 examples/plugin/simple/c/src/plugin.c 里的结构体逐字段一致，
 * ABI 版本 1。改动前请先核对官方 sdk/pluginabi 与 loader。
 *
 * 注意：plugin 的 call 指针用 char* 而不是 const char*，是为了让 cgo 生成的
 * //export 函数签名能直接匹配、免去函数指针强制转换（const 只是编译期限定符，
 * 二进制层面完全相同）。
 */
#ifndef MIMO_CPA_ABI_H
#define MIMO_CPA_ABI_H

#include <stddef.h>
#include <stdint.h>

#define CLIPROXY_ABI_VERSION 1

typedef struct {
    void*  ptr;
    size_t len;
} cliproxy_buffer;

/* ---- 宿主提供给插件的能力表 ---- */
typedef int  (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
    uint32_t               abi_version;
    void*                  host_ctx;
    cliproxy_host_call_fn  call;
    cliproxy_host_free_fn  free_buffer;
} cliproxy_host_api;

/* ---- 插件提供给宿主的函数表 ---- */
typedef int  (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t                  abi_version;
    cliproxy_plugin_call_fn   call;
    cliproxy_plugin_free_fn   free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

/* cgo //export 生成的符号（定义在 _cgo_export.c） */
extern int  cliproxyGoCall(char* method, uint8_t* req, size_t req_len, cliproxy_buffer* out);
extern void cliproxyGoFree(void* ptr, size_t len);
extern void cliproxyGoShutdown(void);

/* bridge.c 实现，供 Go 侧调用 */
int  bridge_host_call(cliproxy_host_api* host, char* method, uint8_t* req, size_t req_len, cliproxy_buffer* out);
void bridge_host_free(cliproxy_host_api* host, void* ptr, size_t len);
void bridge_fill_plugin(cliproxy_plugin_api* api);

#endif /* MIMO_CPA_ABI_H */
