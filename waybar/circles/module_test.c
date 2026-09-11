/* Functional adapter test with real GLib file/pidfd events and a small GTK
 * test double. No display server or graphical client is started by this test. */
#include "module.c"
#include <signal.h>
#include <sys/wait.h>

struct _GtkWidget {
    GObject parent_instance;
    GPtrArray *children;
    GHashTable *classes;
    GtkWidget *parent;
    char *name, *tooltip;
    gboolean visible;
};
typedef struct {GObjectClass parent_class;} GtkWidgetClass;
G_DEFINE_TYPE(GtkWidget, test_widget, G_TYPE_OBJECT)
static void test_widget_finalize(GObject *object) {
    GtkWidget *widget = (GtkWidget *)object;
    g_assert_cmpuint(widget->children->len, ==, 0);
    g_ptr_array_unref(widget->children);
    g_hash_table_destroy(widget->classes);
    g_free(widget->name); g_free(widget->tooltip);
    G_OBJECT_CLASS(test_widget_parent_class)->finalize(object);
}
static void test_widget_class_init(GtkWidgetClass *klass) {
    G_OBJECT_CLASS(klass)->finalize = test_widget_finalize;
    g_signal_new("clicked", G_TYPE_FROM_CLASS(klass), G_SIGNAL_RUN_LAST, 0, NULL, NULL, NULL, G_TYPE_NONE, 0);
}
static void test_widget_init(GtkWidget *widget) {
    widget->children = g_ptr_array_new();
    widget->classes = g_hash_table_new_full(g_str_hash,g_str_equal,g_free,NULL);
}
GtkWidget *gtk_box_new(int orientation,int spacing) {(void)orientation;(void)spacing;return g_object_new(test_widget_get_type(),NULL);}
GtkWidget *gtk_button_new(void) {return gtk_box_new(0,0);}
void gtk_container_add(GtkContainer *parent,GtkWidget *child) {child->parent=(GtkWidget*)parent;g_ptr_array_add(child->parent->children,child);}
void gtk_box_reorder_child(GtkBox *box,GtkWidget *child,int position) {GtkWidget *parent=(GtkWidget*)box;g_ptr_array_remove(parent->children,child);g_ptr_array_insert(parent->children,position,child);}
void gtk_widget_destroy(GtkWidget *widget) {
    while(widget->children->len) gtk_widget_destroy(g_ptr_array_index(widget->children,0));
    if(widget->parent) g_ptr_array_remove(widget->parent->children,widget);
    g_object_unref(widget);
}
void gtk_widget_show(GtkWidget *widget) {widget->visible=TRUE;}
void gtk_widget_hide(GtkWidget *widget) {widget->visible=FALSE;}
void gtk_widget_set_no_show_all(GtkWidget *widget,gboolean value) {(void)widget;(void)value;}
void gtk_widget_set_size_request(GtkWidget *widget,int width,int height) {(void)widget;g_assert_cmpint(width,==,16);g_assert_cmpint(height,==,16);}
void gtk_widget_set_valign(GtkWidget *widget,int value) {(void)widget;(void)value;}
void gtk_widget_set_can_focus(GtkWidget *widget,gboolean value) {(void)widget;(void)value;}
void gtk_widget_set_name(GtkWidget *widget,const char *value) {g_free(widget->name);widget->name=g_strdup(value);}
void gtk_widget_set_tooltip_text(GtkWidget *widget,const char *value) {g_free(widget->tooltip);widget->tooltip=g_strdup(value);}
GtkStyleContext *gtk_widget_get_style_context(GtkWidget *widget) {return (GtkStyleContext*)widget;}
void gtk_style_context_add_class(GtkStyleContext *style,const char *name) {g_hash_table_add(((GtkWidget*)style)->classes,g_strdup(name));}
void gtk_style_context_remove_class(GtkStyleContext *style,const char *name) {g_hash_table_remove(((GtkWidget*)style)->classes,name);}
static GtkContainer *get_root(wbcffi_module *obj) {return (GtkContainer*)obj;}
static void write_frame(const char *path, const char *mode, int count, int publisher, gboolean reverse) {
    GString *frame=g_string_new(NULL);
    g_string_append_printf(frame,"[display]\nversion=1\nmode=%s\nvisible=true\nconnected=true\npublisher_pid=%d\npublisher_started=%lu\ncount=%d\n",mode,publisher,(unsigned long)process_started(publisher),count);
    for(int i=0;i<count;++i) {
        int id=reverse?count-1-i:i;
        g_string_append_printf(frame,"[session-%d]\nkey=session-%d\nselector=host:test:pid:%d:started:2026-09-11T00:00:00Z\ntooltip=session;%d\\nstatus\nclasses=%s;focused;\n",i,id,id,id,reverse?"permission":"working");
    }
    GError *error=NULL;
    g_assert_true(g_file_set_contents_full(path,frame->str,-1,G_FILE_SET_CONTENTS_CONSISTENT,0600,&error));
    g_assert_no_error(error);
    g_string_free(frame,TRUE);
}
static void pump(void) {
    gint64 deadline=g_get_monotonic_time()+300000;
    while(g_get_monotonic_time()<deadline) {
        while(g_main_context_iteration(NULL,FALSE)) {}
        g_usleep(1000);
    }
}
int main(void) {
    GError *error=NULL;
    char *dir=g_dir_make_tmp("switchboard-circles-test-XXXXXX",&error);g_assert_no_error(error);
    char *path=g_build_filename(dir,"view.ini",NULL);
    GtkWidget *root=gtk_box_new(0,0);
    wbcffi_init_info info={.obj=(wbcffi_module*)root,.get_root_widget=get_root};
    wbcffi_config_entry entry={.key="data_file",.value=path};
    Display *display=wbcffi_init(&info,&entry,1);
    g_assert_false(root->visible);
    write_frame(path,"circles",32,getpid(),FALSE);pump();
    g_assert_true(root->visible);
    g_assert_cmpuint(display->box->children->len,==,32);
    Circle *first=g_hash_table_lookup(display->circles,"session-0");
    GtkWidget *original=first->widget;
    g_assert_cmpstr(original->tooltip,==,"session;0\nstatus");
    g_assert_true(g_hash_table_contains(original->classes,"working"));
    write_frame(path,"circles",32,getpid(),TRUE);pump();
    g_assert_true(first->widget==original);
    g_assert_true(g_ptr_array_index(display->box->children,31)==original);
    g_assert_true(g_hash_table_contains(original->classes,"permission"));
    g_assert_false(g_hash_table_contains(original->classes,"working"));
    write_frame(path,"chips",32,getpid(),FALSE);pump();g_assert_false(root->visible);
    write_frame(path,"circles",2,getpid(),FALSE);pump();
    g_assert_true(root->visible);g_assert_cmpuint(display->box->children->len,==,2);
    write_frame(path,"circles",0,getpid(),FALSE);pump();
    g_assert_false(root->visible);g_assert_cmpuint(display->box->children->len,==,0);
    pid_t child=fork();g_assert_cmpint(child,>=,0);
    if(!child) {for(;;) pause();}
    write_frame(path,"circles",3,child,FALSE);pump();g_assert_true(root->visible);
    kill(child,SIGKILL);waitpid(child,NULL,0);pump();g_assert_false(root->visible);
    write_frame(path,"circles",1,getpid(),FALSE);pump();g_assert_true(root->visible);
    wbcffi_deinit(display);gtk_widget_destroy(root);pump();
    unlink(path);rmdir(dir);g_free(path);g_free(dir);
    puts("PASS: live rename, 32 circles, stable identity/reorder, status changes, mode/empty collapse, publisher death/recovery, cleanup");
    return 0;
}
