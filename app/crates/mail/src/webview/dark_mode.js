(function() {
    const DARK_BG = [42, 42, 44];
    const MIN_CONTRAST = 4.5;

    function toLinear(c) {
        c = c / 255;
        return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
    }

    function toSRGB(c) {
        c = Math.max(0, Math.min(1, c));
        return c <= 0.0031308 ? Math.round(c * 12.92 * 255) : Math.round((1.055 * Math.pow(c, 1/2.4) - 0.055) * 255);
    }

    function luminance(r, g, b) {
        return 0.2126 * toLinear(r) + 0.7152 * toLinear(g) + 0.0722 * toLinear(b);
    }

    function contrast(l1, l2) {
        const lighter = Math.max(l1, l2);
        const darker = Math.min(l1, l2);
        return (lighter + 0.05) / (darker + 0.05);
    }

    function rgbToLab(r, g, b) {
        let lr = toLinear(r), lg = toLinear(g), lb = toLinear(b);
        let x = (0.4124564 * lr + 0.3575761 * lg + 0.1804375 * lb) / 0.95047;
        let y = (0.2126729 * lr + 0.7151522 * lg + 0.0721750 * lb) / 1.0;
        let z = (0.0193339 * lr + 0.1191920 * lg + 0.9503041 * lb) / 1.08883;
        const f = (t) => t > 0.008856 ? Math.pow(t, 1/3) : (903.3 * t + 16) / 116;
        x = f(x); y = f(y); z = f(z);
        return [116 * y - 16, 500 * (x - y), 200 * (y - z)];
    }

    function labToRgb(L, a, b) {
        let y = (L + 16) / 116;
        let x = a / 500 + y;
        let z = y - b / 200;
        const finv = (t) => t > 0.206897 ? t * t * t : (116 * t - 16) / 903.3;
        x = finv(x) * 0.95047; y = finv(y) * 1.0; z = finv(z) * 1.08883;
        let lr =  3.2404542 * x - 1.5371385 * y - 0.4985314 * z;
        let lg = -0.9692660 * x + 1.8760108 * y + 0.0415560 * z;
        let lb =  0.0556434 * x - 0.2040259 * y + 1.0572252 * z;
        return [toSRGB(lr), toSRGB(lg), toSRGB(lb)];
    }

    function fixContrast(r, g, b, bgL) {
        const lab = rgbToLab(r, g, b);
        const baseLValue = rgbToLab(DARK_BG[0], DARK_BG[1], DARK_BG[2])[0];
        let newL = lab[0] * ((100 - baseLValue) / 100) + baseLValue;
        const floor = 50 + baseLValue;
        const ceiling = 50;
        const mid = (floor + ceiling) / 2;
        if (bgL > mid) {
            newL = Math.min(newL, 2 * mid - newL);
            newL = ((newL - baseLValue) * (ceiling - baseLValue)) / (mid - baseLValue) + baseLValue;
        } else {
            newL = Math.max(newL, 2 * mid - newL);
            newL = 100 - ((100 - newL) * (100 - floor)) / (100 - mid);
        }
        return labToRgb(newL, lab[1], lab[2]);
    }

    function parseColor(str) {
        if (!str || str === 'transparent' || str === 'rgba(0, 0, 0, 0)') return null;
        const m = str.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
        if (m) return [+m[1], +m[2], +m[3]];
        return null;
    }

    const darkBgLum = luminance(DARK_BG[0], DARK_BG[1], DARK_BG[2]);

    // Check html element
    const htmlBg = parseColor(window.getComputedStyle(document.documentElement).backgroundColor);
    if (htmlBg && luminance(htmlBg[0], htmlBg[1], htmlBg[2]) > 0.3) {
        document.documentElement.style.setProperty('background-color', 'transparent', 'important');
    }

    // Check body
    const bodyBg = parseColor(window.getComputedStyle(document.body).backgroundColor);
    if (bodyBg && luminance(bodyBg[0], bodyBg[1], bodyBg[2]) > 0.3) {
        document.body.style.setProperty('background-color', 'transparent', 'important');
    }

    const els = document.body.querySelectorAll('*');
    for (const el of els) {
        const tag = el.tagName;
        if (tag === 'IMG' || tag === 'VIDEO' || tag === 'IFRAME' || tag === 'SVG') continue;

        const style = window.getComputedStyle(el);

        const bg = parseColor(style.backgroundColor);
        if (bg) {
            const bgLum = luminance(bg[0], bg[1], bg[2]);
            if (bgLum > 0.3) {
                // Light backgrounds → transparent (let dark card show through)
                el.style.setProperty('background-color', 'transparent', 'important');
            } else if (bgLum > 0.15) {
                // Medium-light backgrounds → darken via CIELAB
                const newBg = fixContrast(bg[0], bg[1], bg[2], darkBgLum);
                el.style.setProperty('background-color', 'rgb(' + newBg.join(',') + ')', 'important');
            }
        }

        const fg = parseColor(style.color);
        if (fg) {
            const fgLum = luminance(fg[0], fg[1], fg[2]);
            if (fgLum < 0.4) {
                const newFg = fixContrast(fg[0], fg[1], fg[2], darkBgLum);
                const newFgLum = luminance(newFg[0], newFg[1], newFg[2]);
                if (contrast(newFgLum, darkBgLum) >= MIN_CONTRAST) {
                    el.style.setProperty('color', 'rgb(' + newFg.join(',') + ')', 'important');
                } else {
                    el.style.setProperty('color', '#e5e5e5', 'important');
                }
            }
        }
    }

    const links = document.querySelectorAll('a');
    for (const a of links) {
        const fg = parseColor(window.getComputedStyle(a).color);
        if (fg) {
            const fgLum = luminance(fg[0], fg[1], fg[2]);
            if (fgLum < 0.3) {
                a.style.setProperty('color', '#6ba3f7', 'important');
            }
        }
    }
})();
