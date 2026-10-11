//! Linear draws workflow-state icons procedurally (an SVG with dash-array tricks). We reproduce
//! the same geometry with GPUI paths instead of shipping one asset per state × progress.

use std::f32::consts::{PI, TAU};

use gpui::{
    App, Bounds, Hsla, IntoElement, PathBuilder, Pixels, Point, RenderOnce, Window, canvas, div, point,
    prelude::*, px,
};

use crate::model::{Status, StatusKind};
use ui::theme::ActiveTheme;
use crate::linear_icons::LinearIcon;
use ui::Icon;

const SIZE: f32 = 14.;

fn circle(builder: &mut PathBuilder, center: Point<Pixels>, r: f32) {
    let r = px(r);
    builder.move_to(point(center.x, center.y - r));
    builder.arc_to(point(r, r), px(0.), false, true, point(center.x, center.y + r));
    builder.arc_to(point(r, r), px(0.), false, true, point(center.x, center.y - r));
    builder.close();
}

/// Filled wedge starting at 12 o'clock, clockwise.
fn pie(builder: &mut PathBuilder, center: Point<Pixels>, r: f32, fraction: f32) {
    if fraction >= 0.999 {
        circle(builder, center, r);
        return;
    }
    let angle = fraction * TAU - PI / 2.;
    let end = point(center.x + px(r * angle.cos()), center.y + px(r * angle.sin()));
    builder.move_to(center);
    builder.line_to(point(center.x, center.y - px(r)));
    builder.arc_to(point(px(r), px(r)), px(0.), fraction > 0.5, true, end);
    builder.close();
}

fn paint_ring(bounds: Bounds<Pixels>, color: Hsla, dashed: bool, window: &mut Window) {
    let mut ring = PathBuilder::stroke(px(1.5));
    if dashed {
        // matches `stroke-dasharray="1.4 1.74"` on the r=6 backlog circle
        ring = ring.dash_array(&[px(1.4), px(1.74)]);
    }
    circle(&mut ring, bounds.center(), 6.);
    if let Ok(path) = ring.build() {
        window.paint_path(path, color);
    }
}

fn paint_pie(bounds: Bounds<Pixels>, color: Hsla, r: f32, fraction: f32, window: &mut Window) {
    if fraction <= 0. {
        return;
    }
    let mut builder = PathBuilder::fill();
    pie(&mut builder, bounds.center(), r, fraction);
    if let Ok(path) = builder.build() {
        window.paint_path(path, color);
    }
}

#[derive(IntoElement)]
pub struct StatusIcon {
    status: Status,
}

impl StatusIcon {
    pub fn new(status: &Status) -> Self {
        Self { status: status.clone() }
    }
}

impl RenderOnce for StatusIcon {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let Status { kind, color, progress, .. } = self.status;
        let glyph = match kind {
            StatusKind::Completed => Some(LinearIcon::StatusCheck),
            StatusKind::Canceled => Some(LinearIcon::StatusCross),
            _ => None,
        };

        div()
            .relative()
            .size(px(SIZE))
            .flex_none()
            .child(
                canvas(
                    |_, _, _| {},
                    move |bounds, _, window, _| {
                        paint_ring(bounds, color, kind == StatusKind::Backlog, window);
                        match kind {
                            StatusKind::Completed | StatusKind::Canceled => paint_pie(bounds, color, 6., 1., window),
                            // inner pie: r=2 circle with stroke-width 4 → filled radius 4
                            _ => paint_pie(bounds, color, 4., progress, window),
                        }
                    },
                )
                .size_full(),
            )
            .when_some(glyph, |this, glyph| {
                // the check / cross is a cut-out painted in the surface color
                this.child(div().absolute().inset_0().child(Icon::new(glyph).size(SIZE).color(cx.theme().bg_panel)))
            })
    }
}

/// Sub-issue progress ring (`1/2` chips): grey track with a colored arc.
#[derive(IntoElement)]
pub struct ProgressPie {
    fraction: f32,
    color: Hsla,
}

impl ProgressPie {
    pub fn new(fraction: f32, color: Hsla) -> Self {
        Self { fraction, color }
    }
}

impl RenderOnce for ProgressPie {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let track = cx.theme().border_chip;
        let Self { fraction, color } = self;
        canvas(
            |_, _, _| {},
            move |bounds, _, window, _| {
                let c = bounds.center();
                let mut ring = PathBuilder::stroke(px(1.5));
                circle(&mut ring, c, 5.5);
                if let Ok(path) = ring.build() {
                    window.paint_path(path, track);
                }
                paint_pie(bounds, color, 3.5, fraction, window);
            },
        )
        .size(px(14.))
        .flex_none()
    }
}
