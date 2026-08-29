//go:build linux && tray

// The GTK callbacks live here rather than in the Go file's preamble: a
// preamble beside //export is copied into two translation units, so it may
// hold declarations only.

#include <gtk/gtk.h>
#include "_cgo_export.h"

static void monbooru_tray_on_open(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monbooruTrayOpen();
}

static void monbooru_tray_on_autostart(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monbooruTrayAutostart();
}

static void monbooru_tray_on_quit(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monbooruTrayQuit();
}

void monbooru_tray_connect(GtkWidget *item, int which) {
	switch (which) {
	case 0:
		g_signal_connect(item, "activate", G_CALLBACK(monbooru_tray_on_open), NULL);
		break;
	case 1:
		g_signal_connect(item, "activate", G_CALLBACK(monbooru_tray_on_autostart), NULL);
		break;
	default:
		g_signal_connect(item, "activate", G_CALLBACK(monbooru_tray_on_quit), NULL);
		break;
	}
}
