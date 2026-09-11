#define _GNU_SOURCE
#include "gtk_abi.h"
#include <errno.h>
#include <poll.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

/* v1 delivers string configuration values directly; only strings are used. */
const size_t wbcffi_version = 1;
#ifndef SWITCHBOARD_DISPLAY_REVISION
#define SWITCHBOARD_DISPLAY_REVISION "development"
#endif
const char switchboard_display_revision[] = SWITCHBOARD_DISPLAY_REVISION;
typedef struct Display Display;
typedef struct {
    Display *display;
    GtkWidget *widget;
    char *selector;
    char *tooltip;
    gboolean tooltip_markup;
    char **classes;
    gboolean seen;
} Circle;
struct Display {
    GtkWidget *root, *box;
    GHashTable *circles;
    GFileMonitor *monitor;
    gulong monitor_handler;
    char *path, *ctl;
    int owner_pid, owner_fd;
    guint64 owner_started;
    guint owner_source;
};

void wbcffi_deinit(void *instance);

static void forget_owner(Display *display) {
    if (display->owner_source) g_source_remove(display->owner_source);
    display->owner_source = 0;
    if (display->owner_fd >= 0) close(display->owner_fd);
    display->owner_fd = -1;
    display->owner_pid = 0;
    display->owner_started = 0;
}
static gboolean owner_exited(gint fd, GIOCondition condition, gpointer data) {
    (void)fd; (void)condition;
    Display *display = data;
    display->owner_source = 0;
    forget_owner(display);
    gtk_widget_hide(display->root);
    return G_SOURCE_REMOVE;
}
static guint64 process_started(int pid) {
    char *path = g_strdup_printf("/proc/%d/stat", pid);
    char *stat = NULL;
    guint64 value = 0;
    if (g_file_get_contents(path, &stat, NULL, NULL)) {
        char *field = strrchr(stat, ')');
        if (field) {
            ++field;
            for (int index = 0; index <= 19; ++index) {
                while (g_ascii_isspace(*field)) ++field;
                if (index == 19) { value = g_ascii_strtoull(field, NULL, 10); break; }
                while (*field && !g_ascii_isspace(*field)) ++field;
            }
        }
    }
    g_free(stat); g_free(path);
    return value;
}
static gboolean attach_owner(Display *display, int pid, guint64 started) {
    if (pid <= 0 || !started) return FALSE;
    if (display->owner_pid == pid && display->owner_started == started) return TRUE;
    forget_owner(display);
    int fd = (int)syscall(SYS_pidfd_open, pid, 0);
    if (fd < 0) return FALSE;
    struct pollfd pollfd = {.fd = fd, .events = POLLIN};
    if (poll(&pollfd, 1, 0) != 0 || process_started(pid) != started) { close(fd); return FALSE; }
    display->owner_pid = pid;
    display->owner_started = started;
    display->owner_fd = fd;
    display->owner_source = g_unix_fd_add(fd, G_IO_IN | G_IO_HUP | G_IO_ERR, owner_exited, display);
    return TRUE;
}
static void focus_circle(GtkButton *button, gpointer data) {
    (void)button;
    Circle *circle = data;
    if (!circle->selector || !*circle->selector) return;
    char *argv[] = {circle->display->ctl, "focus", circle->selector, NULL};
    GError *error = NULL;
    /* Pass the stable, generation-scoped selector as one argv element. */
    if (!g_spawn_async(NULL, argv, NULL, 0, NULL, NULL, NULL, &error)) {
        g_warning("switchboard circles: focus: %s", error->message);
        g_clear_error(&error);
    }
}
static void free_circle(gpointer data) {
    Circle *circle = data;
    gtk_widget_destroy(circle->widget);
    g_free(circle->selector);
    g_free(circle->tooltip);
    g_strfreev(circle->classes);
    g_free(circle);
}
static Circle *make_circle(Display *display) {
    Circle *circle = g_new0(Circle, 1);
    circle->display = display;
    circle->widget = gtk_button_new();
    gtk_widget_set_name(circle->widget, "switchboard-circle");
    gtk_widget_set_size_request(circle->widget, 24, 24);
    gtk_widget_set_valign(circle->widget, 3); /* GTK_ALIGN_CENTER */
    gtk_widget_set_can_focus(circle->widget, FALSE);
    gtk_container_add((GtkContainer *)display->box, circle->widget);
    g_signal_connect(circle->widget, "clicked", G_CALLBACK(focus_circle), circle);
    return circle;
}
static void set_classes(Circle *circle, char **classes) {
    if (circle->classes && classes && g_strv_equal((const char *const *)circle->classes, (const char *const *)classes)) {
        g_strfreev(classes);
        return;
    }
    GtkStyleContext *style = gtk_widget_get_style_context(circle->widget);
    if (circle->classes) for (char **item = circle->classes; *item; ++item)
        gtk_style_context_remove_class(style, *item);
    g_strfreev(circle->classes);
    circle->classes = classes;
    if (classes) for (char **item = classes; *item; ++item)
        gtk_style_context_add_class(style, *item);
}
static void set_tooltip(Circle *circle, GKeyFile *frame, const char *group) {
    char *tooltip = g_key_file_get_string(frame, group, "tooltip_markup", NULL);
    gboolean markup = tooltip && *tooltip;
    if (!markup) {
        g_free(tooltip);
        tooltip = g_key_file_get_string(frame, group, "tooltip", NULL);
    }
    /* Other sessions' updates must not disturb an unchanged open hover card. */
    if (markup != circle->tooltip_markup || g_strcmp0(tooltip, circle->tooltip) != 0) {
        if (markup) gtk_widget_set_tooltip_markup(circle->widget, tooltip);
        else gtk_widget_set_tooltip_text(circle->widget, tooltip);
        g_free(circle->tooltip);
        circle->tooltip = tooltip;
        circle->tooltip_markup = markup;
    } else g_free(tooltip);
}
static void render(Display *display) {
    GKeyFile *frame = g_key_file_new();
    if (!g_key_file_load_from_file(frame, display->path, G_KEY_FILE_NONE, NULL) ||
        g_key_file_get_integer(frame, "display", "version", NULL) != 1) {
        gtk_widget_hide(display->root);
        g_key_file_unref(frame);
        return;
    }
    int pid = g_key_file_get_integer(frame, "display", "publisher_pid", NULL);
    guint64 started = g_key_file_get_uint64(frame, "display", "publisher_started", NULL);
    char *mode = g_key_file_get_string(frame, "display", "mode", NULL);
    gboolean show = g_strcmp0(mode, "circles") == 0 &&
        g_key_file_get_boolean(frame, "display", "visible", NULL) &&
        g_key_file_get_boolean(frame, "display", "connected", NULL) &&
        attach_owner(display, pid, started);
    g_free(mode);
    if (!show) { gtk_widget_hide(display->root); g_key_file_unref(frame); return; }
    int count = g_key_file_get_integer(frame, "display", "count", NULL);
    GHashTableIter iter;
    gpointer value;
    g_hash_table_iter_init(&iter, display->circles);
    while (g_hash_table_iter_next(&iter, NULL, &value)) ((Circle *)value)->seen = FALSE;
    for (int i = 0; i < count; ++i) {
        char *group = g_strdup_printf("session-%d", i);
        char *key = g_key_file_get_string(frame, group, "key", NULL);
        if (!key || !*key) {g_free(key); g_free(group); continue;}
        Circle *circle = g_hash_table_lookup(display->circles, key);
        if (!circle) {
            circle = make_circle(display);
            g_hash_table_insert(display->circles, g_strdup(key), circle);
        }
        circle->seen = TRUE;
        g_free(circle->selector);
        circle->selector = g_key_file_get_string(frame, group, "selector", NULL);
        set_tooltip(circle, frame, group);
        set_classes(circle, g_key_file_get_string_list(frame, group, "classes", NULL, NULL));
        gtk_box_reorder_child((GtkBox *)display->box, circle->widget, i);
        gtk_widget_show(circle->widget);
        g_free(group); g_free(key);
    }
    g_hash_table_iter_init(&iter, display->circles);
    while (g_hash_table_iter_next(&iter, NULL, &value))
        if (!((Circle *)value)->seen) g_hash_table_iter_remove(&iter);
    if (g_hash_table_size(display->circles)) {
        gtk_widget_show(display->box);
        gtk_widget_show(display->root);
    } else gtk_widget_hide(display->root);
    g_key_file_unref(frame);
}
static void file_changed(GFileMonitor *monitor, GFile *file, GFile *other,
                         GFileMonitorEvent event, gpointer data) {
    (void)monitor; (void)event;
    Display *display = data;
    char *path = g_file_get_path(file);
    char *destination = other ? g_file_get_path(other) : NULL;
    if (g_strcmp0(path, display->path) == 0 || g_strcmp0(destination, display->path) == 0) render(display);
    g_free(path); g_free(destination);
}
void *wbcffi_init(const wbcffi_init_info *info, const wbcffi_config_entry *entries, size_t len) {
    Display *display = g_new0(Display, 1);
    display->owner_fd = -1;
    display->root = (GtkWidget *)info->get_root_widget(info->obj);
    display->ctl = g_build_filename(g_get_home_dir(), ".local", "bin", "switchboard-ctl", NULL);
    display->path = g_build_filename(g_get_user_runtime_dir(), "switchboard", "waybar-circles.ini", NULL);
    for (size_t i = 0; i < len; ++i) {
        if (strcmp(entries[i].key, "data_file") == 0) {g_free(display->path); display->path = g_strdup(entries[i].value);}
        if (strcmp(entries[i].key, "ctl") == 0) {g_free(display->ctl); display->ctl = g_strdup(entries[i].value);}
    }
    display->circles = g_hash_table_new_full(g_str_hash, g_str_equal, g_free, free_circle);
    display->box = gtk_box_new(0, 4); /* GTK_ORIENTATION_HORIZONTAL */
    gtk_widget_set_name(display->box, "switchboard-circles");
    gtk_container_add((GtkContainer *)display->root, display->box);
    gtk_widget_set_no_show_all(display->root, TRUE);
    gtk_widget_hide(display->root);
    char *directory = g_path_get_dirname(display->path);
    g_mkdir_with_parents(directory, 0700);
    GFile *dir = g_file_new_for_path(directory);
    GError *error = NULL;
    display->monitor = g_file_monitor_directory(dir, G_FILE_MONITOR_WATCH_MOVES, NULL, &error);
    g_object_unref(dir); g_free(directory);
    if (!display->monitor) {
        g_warning("switchboard circles: monitor: %s", error->message);
        g_clear_error(&error);
        wbcffi_deinit(display);
        return NULL;
    } else {
        g_file_monitor_set_rate_limit(display->monitor, 20);
        display->monitor_handler = g_signal_connect(display->monitor, "changed", G_CALLBACK(file_changed), display);
    }
    render(display);
    return display;
}
void wbcffi_deinit(void *instance) {
    Display *display = instance;
    if (!display) return;
    if (display->monitor) {
        g_signal_handler_disconnect(display->monitor, display->monitor_handler);
        g_file_monitor_cancel(display->monitor);
        g_object_unref(display->monitor);
    }
    forget_owner(display);
    g_hash_table_destroy(display->circles);
    gtk_widget_destroy(display->box);
    g_free(display->path); g_free(display->ctl); g_free(display);
}
void wbcffi_update(void *instance) {render(instance);}
void wbcffi_refresh(void *instance, int signal) {(void)instance; (void)signal;}
void wbcffi_doaction(void *instance, const char *action) {(void)instance; (void)action;}
