import importlib.util
import sys
import types
import unittest
from pathlib import Path


def _load_server_module():
    server_path = Path(__file__).with_name("server.py")

    fake_camoufox_pkg = types.ModuleType("camoufox")
    fake_camoufox_async = types.ModuleType("camoufox.async_api")

    class AsyncCamoufox:  # pragma: no cover - 测试只需要占位，避免导入失败
        pass

    fake_camoufox_async.AsyncCamoufox = AsyncCamoufox
    sys.modules.setdefault("camoufox", fake_camoufox_pkg)
    sys.modules["camoufox.async_api"] = fake_camoufox_async

    spec = importlib.util.spec_from_file_location("aws_builder_id_reg_server", server_path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


server = _load_server_module()


class GeminiHelperTests(unittest.TestCase):
    def test_normalize_yydsmail_base_url(self):
        cases = [
            ("", "https://maliapi.215.im"),
            ("https://maliapi.215.im", "https://maliapi.215.im"),
            ("https://maliapi.215.im/", "https://maliapi.215.im"),
            ("https://maliapi.215.im/v1", "https://maliapi.215.im"),
            ("https://maliapi.215.im/v1/", "https://maliapi.215.im"),
        ]

        for raw, want in cases:
            with self.subTest(raw=raw):
                self.assertEqual(server._normalize_yydsmail_base_url(raw), want)

    def test_extract_gemini_xsrf_from_hidden_input(self):
        html = """
        <form>
          <input name="continueUrl" type="hidden" value="https://business.gemini.google">
          <input name="xsrfToken" type="hidden" value="wM-APfg7awG4f9hZGZIIqVnSb64">
        </form>
        """
        self.assertEqual(
            server._extract_gemini_xsrf(html),
            "wM-APfg7awG4f9hZGZIIqVnSb64",
        )

    def test_is_google_email_accepts_workspace_sender(self):
        self.assertTrue(
            server._is_google_email(
                "Gemini for Workspace <workspace-noreply@google.com>",
                "Your Gemini verification code",
            )
        )

    def test_classify_gemini_page_state_verify_code(self):
        state = server._classify_gemini_page_state(
            "https://accountverification.business.gemini.google/v1/verify-oob-code",
            "请输入验证码 我们已将 6 个字符的验证码发送至 test@example.com",
            has_code_input=True,
        )
        self.assertEqual(state, "verify_code")

    def test_classify_gemini_page_state_email_entry(self):
        state = server._classify_gemini_page_state(
            "https://auth.business.gemini.google/login",
            "欢迎使用 Business 版 使用您的工作邮箱登录或创建免费试用账号 使用邮箱继续",
            has_email_input=True,
            has_login_button=True,
        )
        self.assertEqual(state, "email_entry")

    def test_classify_gemini_page_state_signed_in(self):
        state = server._classify_gemini_page_state(
            "https://business.gemini.google/app/cid/abc123?csesidx=hello",
            "",
        )
        self.assertEqual(state, "signed_in")


if __name__ == "__main__":
    unittest.main()
