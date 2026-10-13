//! Native menu entry points, without keyboard equivalents that could bypass user keymaps.
//! Only attach handlers while the workspace owns focus; AppKit validates availability
//! through those handlers, not through keybinding context predicates.

use gpui::{Context, Div, Focusable, Window, actions, prelude::*};

use crate::{MailWorkspace, Mode};

actions!(app_menu, [NewMessage, NewEvent, Find, Mail, Calendar, Refresh, Shortcuts, ReloadKeymaps]);

impl MailWorkspace {
    pub(super) fn app_menu_handlers(&self, view: Div, window: &Window, cx: &mut Context<Self>) -> Div {
        let idle = self.compose.is_none() && self.event_compose.is_none() && self.picker.is_none()
            && self.confirm_delete.is_none() && self.context_menu.is_none() && self.help.is_none()
            && (self.focus_handle.is_focused(window) || self.calendar.read(cx).focus_handle(cx).is_focused(window));
        view.when(idle, |view| view
            .on_action(cx.listener(|_, _: &NewMessage, window, cx| window.dispatch_action(Box::new(crate::NewMessage), cx)))
            .on_action(cx.listener(|_, _: &NewEvent, window, cx| window.dispatch_action(Box::new(crate::NewEvent), cx)))
            .on_action(cx.listener(|this, _: &Find, window, cx| {
                if this.mode != Mode::Inbox { this.set_mode(Mode::Inbox, window, cx); }
                window.dispatch_action(Box::new(crate::FocusSearch), cx);
            }))
            .on_action(cx.listener(|_, _: &Mail, window, cx| window.dispatch_action(Box::new(crate::GoToInbox), cx)))
            .on_action(cx.listener(|_, _: &Calendar, window, cx| window.dispatch_action(Box::new(crate::GoToCalendar), cx)))
            .on_action(cx.listener(|_, _: &Shortcuts, window, cx| window.dispatch_action(Box::new(crate::ToggleShortcutHelp), cx)))
            .when(self.live.is_some(), |view| view
                .on_action(cx.listener(|_, _: &Refresh, window, cx| window.dispatch_action(Box::new(crate::Retry), cx)))
                .on_action(cx.listener(|_, _: &ReloadKeymaps, window, cx| window.dispatch_action(Box::new(crate::ReloadKeymaps), cx))))
        )
    }
}
