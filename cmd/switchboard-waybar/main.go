// Command switchboard-waybar is the standalone/debug wrapper around the
// Waybar chip renderer. The production bottom bar is brokered by
// `switchboard-ctl bottombar watch`, so it does not spawn this command per slot.
package main

import "github.com/tjmisko/switchboard/internal/waybarchip"

func main() { waybarchip.Main() }
