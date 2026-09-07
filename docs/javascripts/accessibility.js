(() => {
  const drawer = document.querySelector("#__drawer");
  const trigger = document.querySelector('header label[for="__drawer"]');
  if (!(drawer instanceof HTMLInputElement) || !(trigger instanceof HTMLLabelElement)) return;

  const navigation = () => document.querySelector('[data-md-type="navigation"]');
  const enhancedTocTriggers = new WeakSet();
  const tocElements = () => ({
    toggle: document.querySelector("#__toc"),
    trigger: document.querySelector('label.md-sidebar-button[for="__toc"]'),
    navigation: document.querySelector('[data-md-type="toc"] nav.md-nav--secondary'),
  });
  const synchronize = () => {
    const sidebar = navigation();
    const mobile = getComputedStyle(trigger).display !== "none";
    trigger.setAttribute("aria-expanded", String(drawer.checked));
    if (sidebar instanceof HTMLElement) sidebar.inert = mobile && !drawer.checked;
  };
  const synchronizeToc = () => {
    const { toggle, trigger, navigation } = tocElements();
    if (
      !(toggle instanceof HTMLInputElement) ||
      !(trigger instanceof HTMLLabelElement) ||
      !(navigation instanceof HTMLElement)
    )
      return;

    const mobile = getComputedStyle(trigger).display !== "none";
    trigger.setAttribute("aria-expanded", String(toggle.checked));
    navigation.inert = mobile && !toggle.checked;
  };
  const enhanceToc = () => {
    const { toggle, trigger, navigation } = tocElements();
    if (
      !(toggle instanceof HTMLInputElement) ||
      !(trigger instanceof HTMLLabelElement) ||
      !(navigation instanceof HTMLElement)
    )
      return;

    navigation.id = "__toc-navigation";
    trigger.tabIndex = 0;
    trigger.setAttribute("role", "button");
    trigger.setAttribute("aria-label", "On this page");
    trigger.setAttribute("aria-controls", navigation.id);
    if (!enhancedTocTriggers.has(trigger)) {
      enhancedTocTriggers.add(trigger);
      trigger.addEventListener("keydown", (event) => {
        // Zensical delegates Enter activation for role=button; labels only need Space parity.
        if (event.key === " ") {
          event.preventDefault();
          trigger.click();
        }
      });
      toggle.addEventListener("change", synchronizeToc);
    }
    synchronizeToc();
  };

  trigger.tabIndex = 0;
  trigger.setAttribute("role", "button");
  trigger.setAttribute("aria-controls", "__navigation");
  trigger.querySelector("svg")?.setAttribute("aria-hidden", "true");
  trigger.addEventListener("keydown", (event) => {
    // Zensical delegates Enter activation for role=button; labels only need Space parity.
    if (event.key === " ") {
      event.preventDefault();
      trigger.click();
    }
  });
  drawer.addEventListener("change", synchronize);
  window.addEventListener("resize", synchronize);
  document$.subscribe(() => {
    const sidebar = navigation();
    if (sidebar instanceof HTMLElement) sidebar.id = "__navigation";
    synchronize();
  });
  synchronize();
  window.addEventListener("resize", synchronizeToc);
  document$.subscribe(enhanceToc);
  enhanceToc();
})();
