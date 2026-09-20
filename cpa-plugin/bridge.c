/*
 * cgo <-> C ABI 桥。
 *
 * 这里只做两件 Go 做不到的事：
 *   1) 通过函数指针调用宿主（Go 无法直接调用 C 函数指针）；
 *   2) 把 Go 的 //export 函数塞进 cliproxy_plugin_api 表。
 */
#include "abi.h"

/* 由 cgo 生成，声明与此处必须逐字一致 */
extern int  cliproxyGoCall(char* method, uint8_t* req, size_t req_len, cliproxy_buffer* out);
extern void cliproxyGoFree(void* ptr, size_t len);
extern void cliproxyGoShutdown(void);

int bridge_host_call(cliproxy_host_api* host, char* method, uint8_t* req, size_t req_len, cliproxy_buffer* out) {
    if (host == NULL || host->call == NULL) {
        return -1;
    }
    return host->call(host->host_ctx, method, req, req_len, out);
}

void bridge_host_free(cliproxy_host_api* host, void* ptr, size_t len) {
    if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
        host->free_buffer(ptr, len);
    }
}

void bridge_fill_plugin(cliproxy_plugin_api* api) {
    if (api == NULL) {
        return;
    }
    api->abi_version = CLIPROXY_ABI_VERSION;
    api->call        = cliproxyGoCall;
    api->free_buffer = cliproxyGoFree;
    api->shutdown    = cliproxyGoShutdown;
}
