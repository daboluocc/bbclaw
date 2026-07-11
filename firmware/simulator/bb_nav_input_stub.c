/* Simulator stub for bb_nav_input.
 *
 * The real bb_nav_input.c pulls in driver/gpio.h + esp_timer/esp_log (physical
 * nav-key scanning), which the SDL host build has no business linking. The only
 * symbol the simulated pages reference is bb_nav_input_inject (bb_page_standby's
 * touch-tap → key bridge), so provide a no-op: in the preview there is no
 * downstream nav consumer to drive, and rendering is all we need. */
#include "bb_nav_input.h"

void bb_nav_input_inject(bb_nav_event_t event) {
  (void)event;
}
