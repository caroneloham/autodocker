import importlib.util
import io
import json
from pathlib import Path
import socket
import socketserver
import struct
import tempfile
import threading
import unittest
from unittest.mock import Mock

spec=importlib.util.spec_from_file_location("bridge",Path(__file__).with_name("bridge.py"))
bridge=importlib.util.module_from_spec(spec)
spec.loader.exec_module(bridge)

class ChatTests(unittest.TestCase):
    def test_only_explicit_real_player_chat_calls_model(self):
        self.assertEqual(bridge.parse_chat('[12:34:56] [Server thread/INFO]: <Steve> @ia Bonjour'),('Steve','Bonjour'))
        self.assertEqual(bridge.parse_chat('[12:34:56] [Server thread/INFO]: [Not Secure] <Alex> @IA Bonjour'),('Alex','Bonjour'))
        self.assertEqual(bridge.parse_chat('[12:34:56 INFO]: <Alex> @ia Bonjour'),('Alex','Bonjour'))
        for line in ['[12:34:56] [Server thread/INFO]: <Steve> Bonjour','[12:34:56] [Server thread/INFO]: [IA] Bonjour','<Steve> @ia Bonjour','[12:34:56] [Server thread/INFO]: <Steve;op> @ia Salut']:
            self.assertIsNone(bridge.parse_chat(line))

    def test_model_output_is_text_and_cannot_inject_game_commands(self):
        command=bridge.tellraw_command('Alex','"\n/op Evil\n{"clickEvent":{"action":"run_command"}}')
        self.assertTrue(command.startswith('tellraw @a '))
        self.assertNotIn('\n',command)
        value=json.loads(command[len('tellraw @a '):])
        self.assertEqual(list(value),['text'])
        self.assertIn('/op Evil',value['text'])
        with self.assertRaises(ValueError):bridge.tellraw_command('@a','bad selector')
        self.assertLess(len(bridge.tellraw_command('Alex','😀'*1000).encode())+10,4096)

    def test_player_history_is_bounded_and_model_alias_is_used(self):
        model=bridge.Model('http://vllm:8000/v1','autodocker-llm','synthetic-secret')
        model.opener=Mock()
        def reply(request,timeout):
            body=json.loads(request.data)
            self.assertEqual(body['model'],'autodocker-llm')
            self.assertNotIn('synthetic-secret',request.data.decode())
            self.assertLessEqual(len(body['messages']),8)
            return io.BytesIO(json.dumps({'choices':[{'message':{'content':'Bonjour'}}]}).encode())
        model.opener.open.side_effect=reply
        for i in range(40):model.answer('Player'+str(i),'Bonjour')
        self.assertEqual(len(model.history),32)
        for i in range(10):model.answer('Player39','Encore')
        self.assertEqual(len(model.history['Player39']),6)

    def test_log_tail_never_replays_old_chat_and_handles_incomplete_lines(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'latest.log'
            path.write_text('old chat\n',encoding='utf-8')
            tail=bridge.LogTail(path)
            self.assertEqual(tail.poll(),[])
            with path.open('ab') as output:output.write(b'new chat')
            self.assertEqual(tail.poll(),[])
            with path.open('ab') as output:output.write(b'\n')
            self.assertEqual(tail.poll(),['new chat\n'])
            with path.open('ab') as output:output.write(b'busy backlog\n')
            tail.discard_pending()
            self.assertEqual(tail.poll(),[])
            tail.file.close()

    def test_rcon_handshake_and_safe_reply_with_fragmented_packets(self):
        captured=[]
        class Handler(socketserver.BaseRequestHandler):
            def receive(self):
                size=struct.unpack('<i',bridge.receive_exact(self.request,4))[0]
                data=bridge.receive_exact(self.request,size)
                return (*struct.unpack('<ii',data[:8]),data[8:-2].decode())
            def handle(self):
                captured.append(self.receive())
                data=struct.pack('<iii',10,1,2)+b'\0\0'
                for part in data:self.request.sendall(bytes([part]))
                captured.append(self.receive())
                bridge.send_packet(self.request,2,0,'sent')
        with socketserver.TCPServer(('127.0.0.1',0),Handler) as server:
            thread=threading.Thread(target=server.handle_request)
            thread.start()
            bridge.send_answer('127.0.0.1',server.server_address[1],'synthetic-rcon','Alex','Bonjour')
            thread.join(timeout=3)
        self.assertEqual(captured[0],(1,3,'synthetic-rcon'))
        self.assertEqual(captured[1][:2],(2,2))
        self.assertEqual(json.loads(captured[1][2][len('tellraw @a '):])['text'],'[IA] Alex : Bonjour')

    def test_invalid_rcon_packets_fail_closed(self):
        left,right=socket.socketpair()
        try:
            right.sendall(struct.pack('<i',1000000))
            with self.assertRaises(ValueError):bridge.read_packet(left)
        finally:left.close();right.close()

if __name__=='__main__':unittest.main()
