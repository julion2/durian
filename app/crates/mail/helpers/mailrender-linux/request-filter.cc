// Runs inside WebKit's sandboxed web process, before a request reaches the network.
#include <webkit2/webkit-web-extension.h>

static gboolean filter_request(WebKitWebPage*, WebKitURIRequest* request, WebKitURIResponse*, gpointer) {
    const char* uri = webkit_uri_request_get_uri(request);
    // CSP in the host-prepared document narrows HTTPS to images, and disables
    // those too by default. Never permit file:, HTTP, WebSockets or custom schemes.
    return !(g_str_has_prefix(uri, "https://") || g_str_has_prefix(uri, "data:") || g_str_equal(uri, "about:blank"));
}

static void page_created(WebKitWebExtension*, WebKitWebPage* page, gpointer) {
    g_signal_connect(page, "send-request", G_CALLBACK(filter_request), nullptr);
    webkit_web_page_send_message_to_view(page, webkit_user_message_new("durian-policy-ready", nullptr), nullptr, nullptr, nullptr);
}

extern "C" G_MODULE_EXPORT void webkit_web_extension_initialize(WebKitWebExtension* extension) {
    g_signal_connect(extension, "page-created", G_CALLBACK(page_created), nullptr);
}
