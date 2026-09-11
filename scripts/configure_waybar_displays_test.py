import importlib.machinery
import importlib.util
from pathlib import Path
import unittest

loader=importlib.machinery.SourceFileLoader('configure',str(Path(__file__).with_name('configure-waybar-displays')))
spec=importlib.util.spec_from_loader(loader.name,loader)
module=importlib.util.module_from_spec(spec)
loader.exec_module(module)
class ConfigTests(unittest.TestCase):
    def test_top_preserves_modules_and_removes_only_unused_timer(self):
        source='// top\n[{"name":"top","modules-right":["tray","custom/task","custom/language"],"custom/task":{"exec":"unused"},"clock":{"tooltip-format":"https://example.test/a/*b*/"}}]'
        result=module.top_config(source,'/release/circles.so','/ctl')
        top=module.jsonc(result)[0]
        self.assertEqual(top['modules-right'],['tray','custom/language','cffi/switchboard'])
        self.assertNotIn('custom/task',top)
        self.assertEqual(top['clock']['tooltip-format'],'https://example.test/a/*b*/')
        self.assertEqual(module.top_config(result,'/release/circles.so','/ctl'),result)
    def test_style_preserves_chip_palette(self):
        result=module.style_config('#tray,\n#custom-task,\n#clock { padding: 4px; }\n#custom-task { padding-right: 15px; }\n#custom-claude-0 { border: 1px solid green; }\n')
        self.assertNotIn('#custom-task',result)
        self.assertIn('#custom-claude-0 { border: 1px solid green; }',result)
        self.assertEqual(module.style_config(result),result)
    def test_lua_binding_preserves_navigation_and_detects_conflicts(self):
        source='local mainMod = "SUPER"\nhl.bind(mainMod .. " + ALT + Left", hl.dsp.exec_cmd("/ctl/switchboard-ctl cycle prev"))\n'
        result=module.hypr_config(source,'/ctl/switchboard-ctl','SUPER + SHIFT + B')
        self.assertIn('cycle prev',result)
        self.assertIn('display mode toggle',result)
        self.assertEqual(module.hypr_config(result,'/ctl/switchboard-ctl','SUPER + SHIFT + B'),result)
        conflict=source+'hl.bind(mainMod .. " + SHIFT + B", hl.dsp.exec_cmd("other"))\n'
        with self.assertRaisesRegex(ValueError,'already bound'):module.hypr_config(conflict,'/ctl','SUPER + SHIFT + B')
if __name__=='__main__':unittest.main()
