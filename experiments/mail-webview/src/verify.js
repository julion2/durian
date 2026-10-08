// Executed in real WebKit, not Chromium or a DOM mock. Synthetic fixture only.
const violations = [];
addEventListener('securitypolicyviolation', event => violations.push(event.effectiveDirective));
addEventListener('load', () => setTimeout(() => {
    const card = document.querySelector('.card');
    const grid = document.querySelector('.checks');
    const inline = document.querySelector('#inline');
    const dark = document.documentElement.style.colorScheme === 'dark';
    const narrow = innerWidth < 600;
    const first = grid.children[0].getBoundingClientRect();
    const second = grid.children[1].getBoundingClientRect();
    const selection = getSelection();
    const range = document.createRange();
    range.selectNodeContents(document.querySelector('h1'));
    selection.removeAllRanges();
    selection.addRange(range);
    scrollTo(0, document.documentElement.scrollHeight);
    const checks = {
        cssGrid: getComputedStyle(grid).display === 'grid',
        cssGap: getComputedStyle(grid).columnGap === '24px',
        columnGeometry: narrow ? Math.abs(second.left - first.left) < 1 && Math.abs(second.top - first.bottom - 24) < 1 : Math.abs(second.left - first.right - 24) < 1,
        selectableText: selection.toString() === 'The layout is ready. Three things to check.',
        scrollable: scrollY > 0,
        cidDecoded: inline.complete && inline.naturalWidth > 0,
        imageNotInverted: getComputedStyle(inline).filter === 'none',
        noSenderScript: !window.SENDER_SCRIPT_EXECUTED,
        noActiveElements: !document.querySelector('script, iframe, form, input, svg, link, base'),
        remoteImageRemoved: !document.querySelector('#tracker').hasAttribute('src'),
        noLoadedRemoteResources: performance.getEntriesByType('resource').filter(e => /^https?:/.test(e.name)).every(e => !e.transferSize && !e.decodedBodySize),
        theme: dark ? getComputedStyle(document.body).backgroundColor === 'rgb(42, 42, 44)' : getComputedStyle(card).backgroundColor === 'rgb(255, 255, 255)',
        darkText: !dark || getComputedStyle(document.querySelector('#paragraph')).color !== 'rgb(34, 34, 34)',
        cspBlocksCss: violations.some(rule => rule.startsWith('style-src')),
        cspBlocksImages: violations.includes('img-src'),
    };
    window.ipc.postMessage('verify:' + JSON.stringify({checks, dark, narrow, engine: navigator.userAgent}));
}, 600));
