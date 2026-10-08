"""Platform-independent tests of evidence interpretation, not macOS UI tests."""
import importlib.util
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "diagnosis", pathlib.Path(__file__).with_name("diagnose-keyboard.py"))
diagnosis = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnosis)


def event(stage, **details):
    return {"stage": stage, "details": details}


class DiagnosisTests(unittest.TestCase):
    def test_query_copy_does_not_depend_on_ax_or_trace(self):
        for ax in [{"available": False}, {"available": True, "search": False},
                   {"available": True, "search": True, "search_editor_focused": False}]:
            with self.subTest(ax=ax):
                self.assertEqual(diagnosis.classify(True, ax, [], False),
                                 "QUERY_VERIFIED_AX_UNVERIFIED")
        self.assertEqual(diagnosis.classify(True, {
            "available": True, "search": True, "search_editor_focused": True}, [], False),
            "QUERY_VERIFIED")

    def test_search_label_alone_does_not_verify_input(self):
        self.assertEqual(diagnosis.classify(False, {"search": True}, [], False),
                         "SEARCH_OBSERVED_INPUT_UNVERIFIED")
        self.assertEqual(diagnosis.classify(False, {}, [
            event("search.open"), event("search.input", probe_matches=False)], True),
            "SEARCH_OBSERVED_INPUT_UNVERIFIED")

    def test_trace_query_does_not_verify_copy(self):
        self.assertEqual(diagnosis.classify(False, {}, [
            event("search.input", probe_matches=True)], True), "QUERY_RECEIVED_COPY_UNVERIFIED")

    def test_last_observed_boundary_and_unrelated_keys(self):
        native = event("native.key", slash=True, route_to_gpui=True)
        received = event("gpui.received", slash=True)
        cases = [([], "INPUT_UNOBSERVED"),
                 ([event("native.key", slash=False)], "INPUT_UNOBSERVED"),
                 ([native], "NATIVE_RECEIVED_GPUI_UNOBSERVED"),
                 ([native, event("gpui.received", slash=False)], "NATIVE_RECEIVED_GPUI_UNOBSERVED"),
                 ([native, received], "GPUI_RECEIVED_SEARCH_UNOBSERVED"),
                 ([native, received, event("search.open")], "SEARCH_OBSERVED_INPUT_UNVERIFIED")]
        for events, expected in cases:
            with self.subTest(events=events):
                self.assertEqual(diagnosis.classify(False, {}, events, True), expected)
        self.assertEqual(diagnosis.classify(False, {}, [], False), "UNVERIFIED_NO_TRACE")

    def test_log_offset_excludes_earlier_phase_and_partial_line(self):
        with tempfile.TemporaryDirectory(prefix="gpui-trace-test-") as directory:
            path = pathlib.Path(directory) / "native.log"
            before = b'DURIAN_INPUT {"stage":"search.open"}\n'
            after = b'other log\nDURIAN_INPUT {"stage":"native.key"}\nDURIAN_INPUT {"stage":'
            path.write_bytes(before + after)
            self.assertEqual(diagnosis.traces(path, len(before)), [{"stage": "native.key"}])


if __name__ == "__main__":
    unittest.main()
