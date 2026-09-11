/* Opaque GTK 3 ABI used by this Waybar extension. No GTK structures are
 * accessed. GLib/GIO development headers plus Waybar's existing GTK runtime
 * suffice to build it; a GTK development SDK is not required on the host.
 * Function contracts: https://docs.gtk.org/gtk3/ and Waybar's CFFI v1 ABI. */
#pragma once
#include <gio/gio.h>
#include <glib-unix.h>

typedef struct _GtkWidget GtkWidget;
typedef struct _GtkContainer GtkContainer;
typedef struct _GtkBox GtkBox;
typedef struct _GtkButton GtkButton;
typedef struct _GtkStyleContext GtkStyleContext;
extern GtkWidget *gtk_box_new(int orientation, int spacing);
extern GtkWidget *gtk_button_new(void);
extern void gtk_container_add(GtkContainer *, GtkWidget *);
extern void gtk_box_reorder_child(GtkBox *, GtkWidget *, int);
extern void gtk_widget_destroy(GtkWidget *);
extern void gtk_widget_show(GtkWidget *);
extern void gtk_widget_hide(GtkWidget *);
extern void gtk_widget_set_no_show_all(GtkWidget *, gboolean);
extern void gtk_widget_set_size_request(GtkWidget *, int, int);
extern void gtk_widget_set_valign(GtkWidget *, int);
extern void gtk_widget_set_can_focus(GtkWidget *, gboolean);
extern void gtk_widget_set_name(GtkWidget *, const char *);
extern void gtk_widget_set_tooltip_text(GtkWidget *, const char *);
extern GtkStyleContext *gtk_widget_get_style_context(GtkWidget *);
extern void gtk_style_context_add_class(GtkStyleContext *, const char *);
extern void gtk_style_context_remove_class(GtkStyleContext *, const char *);

typedef struct wbcffi_module wbcffi_module;
typedef struct {
    wbcffi_module *obj;
    const char *waybar_version;
    GtkContainer *(*get_root_widget)(wbcffi_module *);
    void (*queue_update)(wbcffi_module *);
} wbcffi_init_info;
typedef struct { const char *key; const char *value; } wbcffi_config_entry;
