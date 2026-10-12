// WebKitGTK implementation of mailrender's existing NDJSON snapshot contract.
// All GTK/WebKit work stays in this helper, never GPUI's event loop. JavaScriptCore
// (already part of WebKit) parses/quotes JSON; it never evaluates input as code.
#include <webkit2/webkit2.h>
#include <glib-unix.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cmath>
#include <cstdlib>
#include <fcntl.h>
#include <iostream>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <unistd.h>

constexpr size_t MAX_FRAME = 64 << 20; // includes JSON escaping and embedded images
constexpr int MAX_HEIGHT = 16000;
constexpr int STRIP_HEIGHT = 4000;
constexpr int WIDTH = 664;
constexpr size_t POOL_SIZE = 3;

static JSCContext* json_context;
static bool ready = false;

static std::string quoted(const std::string& text) {
    if (!json_context) json_context = jsc_context_new();
    g_autoptr(JSCValue) value = jsc_value_new_string(json_context, text.c_str());
    g_autofree char* json = jsc_value_to_json(value, 0);
    return json ? json : "null";
}

static void reply(const std::string& json) {
    std::cout << json << std::endl;
}

[[noreturn]] static void fail(const char* reason) {
    // A single terminal failure also invalidates other in-flight jobs. The owner
    // kills/reaps the process group on EOF; it never replays the work implicitly.
    reply("{\"ready\":false,\"error\":" + quoted(reason) + "}");
    std::exit(1);
}

static std::string string_property(JSCValue* object, const char* key) {
    g_autoptr(JSCValue) value = jsc_value_object_get_property(object, key);
    if (!jsc_value_is_string(value)) fail("invalid renderer request");
    g_autofree char* text = jsc_value_to_string(value);
    return text;
}

struct Worker {
    GtkWidget* window = nullptr;
    WebKitWebView* web = nullptr;
    bool policy_ready = false;
    bool loaded = false;
    bool expecting_load = true;
    bool busy = false;
    std::string id, out, links;
    int height = 1;
    bool clipped = false;
    gint64 started = 0;
};

static std::array<Worker, POOL_SIZE> workers;

static void check_ready() {
    if (ready) return;
    for (const auto& worker : workers) {
        if (!worker.policy_ready || !worker.loaded) return;
    }
    ready = true;
    reply("{\"ready\":true,\"pool\":3}");
}

static gboolean policy_message(WebKitWebView*, WebKitUserMessage* message, gpointer data) {
    if (!g_str_equal(webkit_user_message_get_name(message), "durian-policy-ready")) return FALSE;
    static_cast<Worker*>(data)->policy_ready = true;
    check_ready();
    return TRUE;
}

static gboolean navigation(WebKitWebView*, WebKitPolicyDecision* decision, WebKitPolicyDecisionType type, gpointer data) {
    if (g_getenv("MAILRENDER_DEBUG")) g_printerr("[HTML] navigation type=%d\n", type);
    if (type == WEBKIT_POLICY_DECISION_TYPE_RESPONSE) return FALSE;
    auto* worker = static_cast<Worker*>(data);
    auto* action = webkit_navigation_policy_decision_get_navigation_action(WEBKIT_NAVIGATION_POLICY_DECISION(decision));
    const char* uri = webkit_uri_request_get_uri(webkit_navigation_action_get_request(action));
    if (type == WEBKIT_POLICY_DECISION_TYPE_NAVIGATION_ACTION && worker->expecting_load && g_str_equal(uri, "about:blank")) {
        worker->expecting_load = false;
        webkit_policy_decision_use(decision);
    } else {
        webkit_policy_decision_ignore(decision);
    }
    return TRUE;
}

static bool save_strip(GdkPixbuf* pixels, const std::string& path) {
    g_autofree char* bytes = nullptr;
    gsize length = 0;
    if (!gdk_pixbuf_save_to_buffer(pixels, &bytes, &length, "jpeg", nullptr, "quality", "88", nullptr)) return false;
    // Atomic replacement supports two identical documents sharing a cache path.
    // Never follow a preexisting output symlink or publish a partially encoded JPEG.
    std::string temporary = path + ".XXXXXX";
    int fd = g_mkstemp_full(temporary.data(), O_WRONLY | O_CLOEXEC, 0600);
    if (fd < 0) return false;
    size_t offset = 0;
    while (offset < length) {
        ssize_t written = write(fd, bytes + offset, length - offset);
        if (written < 0 && errno == EINTR) continue;
        if (written <= 0) break;
        offset += written;
    }
    bool ok = close(fd) == 0 && offset == length;
    if (ok) ok = rename(temporary.c_str(), path.c_str()) == 0;
    if (!ok) unlink(temporary.c_str());
    return ok;
}

static void snapshot_done(GObject* object, GAsyncResult* result, gpointer data) {
    auto* worker = static_cast<Worker*>(data);
    cairo_surface_t* surface = webkit_web_view_get_snapshot_finish(WEBKIT_WEB_VIEW(object), result, nullptr);
    if (!surface || cairo_surface_status(surface) != CAIRO_STATUS_SUCCESS) fail("snapshot failed");
    // The visible viewport is bounded, unlike FULL_DOCUMENT (sender CSS can make
    // scrollHeight enormous). Crop strips in CSS pixels regardless of display DPI.
    std::ostringstream strips;
    strips << '[';
    for (int y = 0, index = 0; y < worker->height; y += STRIP_HEIGHT, ++index) {
        int height = std::min(STRIP_HEIGHT, worker->height - y);
        auto* tile = cairo_image_surface_create(CAIRO_FORMAT_RGB24, WIDTH, height);
        auto* cr = cairo_create(tile);
        cairo_set_source_rgb(cr, 1, 1, 1);
        cairo_paint(cr);
        cairo_set_source_surface(cr, surface, 0, -y);
        cairo_paint(cr);
        cairo_destroy(cr);
        g_autoptr(GdkPixbuf) pixels = gdk_pixbuf_get_from_surface(tile, 0, 0, WIDTH, height);
        cairo_surface_destroy(tile);
        std::string path = worker->out.substr(0, worker->out.rfind('.')) + "." + std::to_string(index) + ".jpg";
        if (!pixels || !save_strip(pixels, path)) fail("snapshot write failed");
        if (index) strips << ',';
        strips << "{\"path\":" << quoted(path) << ",\"y\":" << y << ",\"height\":" << height << '}';
    }
    cairo_surface_destroy(surface);
    strips << ']';
    // Integer milliseconds keep the JSON locale-independent after GTK's setlocale.
    reply("{\"id\":" + quoted(worker->id) + ",\"ok\":true,\"width\":664,\"height\":" + std::to_string(worker->height)
        + ",\"clipped\":" + (worker->clipped ? "true" : "false") + ",\"strips\":" + strips.str()
        + ",\"links\":" + worker->links + ",\"ms\":" + std::to_string((g_get_monotonic_time() - worker->started) / 1000) + "}");
    worker->busy = false;
    worker->id.clear();
    worker->links.clear();
}

static gboolean snapshot(gpointer data) {
    auto* worker = static_cast<Worker*>(data);
    if (gtk_widget_get_allocated_width(GTK_WIDGET(worker->web)) != WIDTH ||
        gtk_widget_get_allocated_height(GTK_WIDGET(worker->web)) != worker->height) return G_SOURCE_CONTINUE;
    webkit_web_view_get_snapshot(worker->web, WEBKIT_SNAPSHOT_REGION_VISIBLE, WEBKIT_SNAPSHOT_OPTIONS_NONE, nullptr, snapshot_done, data);
    return G_SOURCE_REMOVE;
}

static void measured(GObject* object, GAsyncResult* result, gpointer data) {
    auto* worker = static_cast<Worker*>(data);
    g_autoptr(JSCValue) value = webkit_web_view_evaluate_javascript_finish(WEBKIT_WEB_VIEW(object), result, nullptr);
    if (!value || !jsc_value_is_object(value)) fail("measurement failed");
    g_autoptr(JSCValue) height = jsc_value_object_get_property(value, "height");
    double measured_height = jsc_value_to_double(height);
    if (!std::isfinite(measured_height) || measured_height < 1) fail("invalid page height");
    worker->height = static_cast<int>(std::min(std::ceil(measured_height), double(MAX_HEIGHT)));
    worker->clipped = measured_height > MAX_HEIGHT;
    g_autoptr(JSCValue) links = jsc_value_object_get_property(value, "links");
    g_autofree char* links_json = jsc_value_to_json(links, 0);
    if (!links_json || strlen(links_json) > (8 << 20) - 4096) fail("too much link metadata");
    worker->links = links_json;
    gtk_widget_set_size_request(GTK_WIDGET(worker->web), WIDTH, worker->height);
    gtk_window_resize(GTK_WINDOW(worker->window), WIDTH, worker->height);
    // An idle callback can run before GTK's next frame allocation, yielding only
    // the previous one-pixel viewport. Wait for the actual bounded allocation.
    g_timeout_add(16, snapshot, worker);
}

static void loaded(WebKitWebView* web, WebKitLoadEvent event, gpointer data) {
    if (g_getenv("MAILRENDER_DEBUG")) g_printerr("[HTML] load event=%d\n", event);
    if (event != WEBKIT_LOAD_FINISHED) return;
    auto* worker = static_cast<Worker*>(data);
    if (!worker->busy) {
        worker->loaded = true;
        check_ready();
        return;
    }
    // Privileged, isolated host code. Markup JS is disabled separately; no input
    // is interpolated here. Cap metadata before crossing the process boundary.
    const char* measure = R"JS((() => {
        const links = [];
        outer: for (const a of document.querySelectorAll('a[href]')) {
            if (a.href.length > 4096) continue;
            for (const r of a.getClientRects()) {
                if (links.length >= 16384) break outer;
                if (r.top < 16000) links.push({x:r.left,y:r.top+scrollY,w:r.width,h:r.height,href:a.href});
            }
        }
        return {height:Math.max(1,document.documentElement.scrollHeight,document.body.scrollHeight),links};
    })())JS";
    webkit_web_view_evaluate_javascript(web, measure, -1, "durian-host", nullptr, nullptr, measured, worker);
}

static void start_worker(Worker& worker, WebKitWebContext* context) {
    worker.window = gtk_offscreen_window_new();
    worker.web = WEBKIT_WEB_VIEW(webkit_web_view_new_with_context(context));
    auto* settings = webkit_web_view_get_settings(worker.web);
    webkit_settings_set_enable_javascript_markup(settings, FALSE);
    webkit_settings_set_enable_html5_local_storage(settings, FALSE);
    webkit_settings_set_enable_html5_database(settings, FALSE);
#if !WEBKIT_CHECK_VERSION(2, 48, 0)
    webkit_settings_set_enable_dns_prefetching(settings, FALSE);
#endif
#if !WEBKIT_CHECK_VERSION(2, 50, 0)
    webkit_settings_set_enable_hyperlink_auditing(settings, FALSE);
#endif
    webkit_settings_set_enable_webgl(settings, FALSE);
    webkit_settings_set_enable_media(settings, FALSE);
    webkit_settings_set_hardware_acceleration_policy(settings, WEBKIT_HARDWARE_ACCELERATION_POLICY_NEVER);
    g_signal_connect(worker.web, "user-message-received", G_CALLBACK(policy_message), &worker);
    g_signal_connect(worker.web, "decide-policy", G_CALLBACK(navigation), &worker);
    g_signal_connect(worker.web, "load-changed", G_CALLBACK(loaded), &worker);
    g_signal_connect(worker.web, "load-failed", G_CALLBACK(+[](WebKitWebView*, WebKitLoadEvent, const char*, GError*, gpointer) -> gboolean {
        fail("document load failed");
    }), nullptr);
    g_signal_connect(worker.web, "web-process-terminated", G_CALLBACK(+[](WebKitWebView*, WebKitWebProcessTerminationReason, gpointer) {
        fail("web process terminated");
    }), nullptr);
    g_signal_connect(worker.web, "permission-request", G_CALLBACK(+[](WebKitWebView*, WebKitPermissionRequest* request, gpointer) -> gboolean {
        webkit_permission_request_deny(request);
        return TRUE;
    }), nullptr);
    g_signal_connect(worker.web, "authenticate", G_CALLBACK(+[](WebKitWebView*, WebKitAuthenticationRequest* request, gpointer) -> gboolean {
        webkit_authentication_request_cancel(request);
        return TRUE;
    }), nullptr);
    gtk_widget_set_size_request(GTK_WIDGET(worker.web), WIDTH, 1);
    gtk_container_add(GTK_CONTAINER(worker.window), GTK_WIDGET(worker.web));
    gtk_widget_show_all(worker.window);
    webkit_web_view_load_html(worker.web, "<!doctype html><p>Ready</p>", "about:blank");
}

static void submit(const std::string& line) {
    if (!ready) fail("request before readiness");
    g_autoptr(JSCValue) request = jsc_value_new_from_json(json_context, line.c_str());
    if (!request || !jsc_value_is_object(request) || jsc_context_get_exception(json_context)) fail("invalid request JSON");
    g_autoptr(JSCValue) width = jsc_value_object_get_property(request, "width");
    if (!jsc_value_is_number(width) || jsc_value_to_double(width) != WIDTH) fail("unsupported render width");
    auto worker = std::find_if(workers.begin(), workers.end(), [](const Worker& w) { return !w.busy; });
    if (worker == workers.end()) fail("renderer queue full");
    worker->id = string_property(request, "id");
    worker->out = string_property(request, "out");
    auto html = string_property(request, "html");
    if (worker->id.size() > 4096 || html.size() > (32 << 20) || worker->out.empty() || worker->out[0] != '/' || worker->out.size() > 4096) fail("request too large or invalid output");
    worker->busy = true;
    worker->started = g_get_monotonic_time();
    worker->expecting_load = true;
    gtk_widget_set_size_request(GTK_WIDGET(worker->web), WIDTH, 1);
    gtk_window_resize(GTK_WINDOW(worker->window), WIDTH, 1);
    webkit_web_view_load_html(worker->web, html.c_str(), "about:blank");
}

static gboolean input(gint fd, GIOCondition, gpointer) {
    static std::string pending;
    char buffer[8192];
    // Bound each dispatch too, so a producer cannot starve the render/deadline loop.
    for (int turn = 0; turn < 128; ++turn) {
        ssize_t n = read(fd, buffer, sizeof buffer);
        if (n == 0) std::exit(0);
        if (n < 0) {
            if (errno == EINTR) continue;
            if (errno == EAGAIN) return G_SOURCE_CONTINUE;
            fail("input failed");
        }
        pending.append(buffer, n);
        if (pending.size() > MAX_FRAME) fail("input frame too large");
        size_t end;
        while ((end = pending.find('\n')) != std::string::npos) {
            submit(pending.substr(0, end));
            pending.erase(0, end + 1);
        }
    }
    return G_SOURCE_CONTINUE;
}

int main(int argc, char** argv) {
    umask(0077);
    // Never silently weaken the engine sandbox, including through inherited env.
    if (g_getenv("WEBKIT_DISABLE_SANDBOX_THIS_IS_DANGEROUS")) fail("sandbox disabling is not supported");
    if (!gtk_init_check(&argc, &argv)) fail("GTK display unavailable");
    char executable[4096];
    ssize_t length = readlink("/proc/self/exe", executable, sizeof(executable) - 1);
    if (length <= 0) fail("helper location unavailable");
    executable[length] = 0;
    g_autofree char* parent = g_path_get_dirname(executable);
    std::string extensions = std::string(parent) + "/mailrender-extensions";
    if (!g_file_test((extensions + "/libdurian-policy.so").c_str(), G_FILE_TEST_IS_REGULAR)) fail("request filter unavailable");
    g_autoptr(WebKitWebContext) context = webkit_web_context_new_ephemeral();
    webkit_web_context_set_sandbox_enabled(context, TRUE);
    webkit_web_context_add_path_to_sandbox(context, extensions.c_str(), TRUE);
    webkit_web_context_set_web_extensions_directory(context, extensions.c_str());
    webkit_cookie_manager_set_accept_policy(webkit_web_context_get_cookie_manager(context), WEBKIT_COOKIE_POLICY_ACCEPT_NEVER);
    g_signal_connect(context, "download-started", G_CALLBACK(+[](WebKitWebContext*, WebKitDownload* download, gpointer) {
        webkit_download_cancel(download);
    }), nullptr);
    for (auto& worker : workers) start_worker(worker, context);
    // WebKit must initialize its event loop before the standalone JSC parser;
    // creating a JSC context first stalls WebKit loads even with a mapped view.
    json_context = jsc_context_new();
    gint64 started = g_get_monotonic_time();
    g_timeout_add(100, +[](gpointer data) -> gboolean {
        gint64 now = g_get_monotonic_time();
        if (!ready && now - *static_cast<gint64*>(data) > 8 * G_USEC_PER_SEC) {
            for (auto& worker : workers) g_printerr("[HTML] startup policy=%d loaded=%d\n", worker.policy_ready, worker.loaded);
            fail("engine readiness timed out");
        }
        for (auto& worker : workers) {
            if (worker.busy && now - worker.started > 15 * G_USEC_PER_SEC) fail("render timed out");
        }
        return G_SOURCE_CONTINUE;
    }, &started);
    if (fcntl(STDIN_FILENO, F_SETFL, fcntl(STDIN_FILENO, F_GETFL) | O_NONBLOCK) < 0) fail("input setup failed");
    g_unix_fd_add(STDIN_FILENO, GIOCondition(G_IO_IN | G_IO_HUP | G_IO_ERR), input, nullptr);
    gtk_main();
    return 0;
}
