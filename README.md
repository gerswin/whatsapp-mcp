# WhatsApp MCP

Conecta tus cuentas de WhatsApp a **Claude**, **ChatGPT** u otro cliente [MCP](https://modelcontextprotocol.io), para que el asistente lea tus chats, vea las fotos, entienda las notas de voz y envíe mensajes por ti.

Es un solo binario en Go sobre [whatsmeow](https://github.com/tulir/whatsmeow), con SQLite. No necesita PostgreSQL ni ningún otro servicio.

- **Varias cuentas** en el mismo servidor; cada conexión de un asistente ve solo una.
- **Historial local**: guarda el texto de los mensajes para listarlos, leerlos y buscarlos.
- **Imágenes**: el asistente puede abrir fotos y stickers y verlos.
- **Notas de voz**: el asistente puede enviar notas de voz leídas con una voz de ElevenLabs (opcional).
- **Audios**: transcripción con Whisper (o cualquier API compatible con OpenAI). Las notas de voz aparecen como texto en el historial y en la búsqueda.
- **OAuth 2.1 integrado**, que es lo que piden los conectores de Claude.ai y ChatGPT.

## Puesta en marcha

```bash
cp .env.example .env      # ajusta PUBLIC_URL; opcional: WHISPER_API_KEY (leer audios) y ELEVENLABS_* (enviar notas de voz)
docker compose up -d --build
```

Vincula una cuenta (el nombre es libre: letras minúsculas, números, `-` y `_`):

```bash
docker exec -it wa-mcp wa-mcp pair personal
```

Se muestra un QR en la terminal. En el teléfono: WhatsApp → Ajustes → Dispositivos vinculados → Vincular un dispositivo. Si no puedes escanear, usa un código:

```bash
docker exec -it wa-mcp wa-mcp pair personal --phone 573001234567
```

Al terminar se imprime **el token de la cuenta, una sola vez**. Guárdalo: es lo que autoriza a un asistente a usar esa cuenta.

Otros comandos:

| Comando | Qué hace |
|---|---|
| `wa-mcp accounts` | Lista las cuentas y si están conectadas |
| `wa-mcp token <nombre>` | Emite un token nuevo (el anterior deja de servir) |
| `wa-mcp remove <nombre>` | Desvincula la cuenta, borra sus mensajes y revoca sus autorizaciones |
| `wa-mcp pair <nombre> --light` | Dibuja el QR para terminales de fondo claro |

## Conectar un asistente

El endpoint es `https://tu-dominio/mcp`.

**Claude.ai y ChatGPT** — agrega un conector personalizado con esa URL. Se abre una página de autorización donde pegas el token de la cuenta. Para usar dos cuentas, agrega el conector dos veces, una con cada token.

**Claude Code**

```bash
claude mcp add --transport http whatsapp https://tu-dominio/mcp \
  --header "Authorization: Bearer <token-de-la-cuenta>"
```

OAuth exige que el servidor sea accesible por HTTPS público. Con un túnel o proxy solo hay que exponer estas rutas:

```yaml
# cloudflared
ingress:
  - hostname: wa.example.com
    path: ^/(mcp|oauth/.*|\.well-known/oauth-.*)$
    service: http://localhost:4020
  - service: http_status:404
```

## Tools

| Tool | Qué hace |
|---|---|
| `get_status` | Estado de conexión y número vinculado |
| `list_chats` | Chats por actividad reciente, con vista previa |
| `get_messages` | Mensajes de un chat, paginable hacia atrás, con sus reacciones y los resultados de las encuestas |
| `search_messages` | Búsqueda de texto, incluidas las transcripciones |
| `get_media` | Abre el archivo de un mensaje: imagen o sticker como imagen; audio, nota de voz o video como transcripción; documentos de texto como texto |
| `list_contacts` | Contactos de la agenda |
| `check_numbers` | Verifica qué números tienen WhatsApp |
| `list_groups` | Grupos de la cuenta |
| `get_group_info` | Nombre, descripción y participantes de un grupo |
| `list_channels` | Canales que sigue la cuenta |
| `get_channel_messages` | Publicaciones de un canal, consultadas a WhatsApp en el momento |
| `send_text` | Envía texto, opcionalmente citando un mensaje |
| `send_media` | Envía imagen, video, audio o documento desde una URL pública |
| `send_voice_note` | Envía una nota de voz leyendo un texto con una voz de ElevenLabs (solo si está configurado) |
| `send_location` | Envía una ubicación |
| `send_poll` | Envía una encuesta |
| `react_to_message` | Reacciona con un emoji |
| `mark_read` | Marca mensajes como leídos |

## Imágenes y audios

Los archivos **no se guardan** en el servidor. De cada mensaje con archivo se guarda una referencia (ruta y claves) y el archivo se descarga de WhatsApp cuando el asistente lo pide con `get_media`. WhatsApp borra los archivos de sus servidores pasado un tiempo, así que los muy antiguos pueden no estar disponibles.

- **Imágenes y stickers**: se reducen a un máximo de 1568 px y se entregan como JPEG.
- **Audios y notas de voz**: con `WHISPER_API_KEY` definida se transcriben. Con `WHISPER_AUTO=true` (por defecto) cada audio que llega, de hasta `WHISPER_MAX_SECONDS`, se transcribe en el momento y el texto queda en el historial. Los del historial sincronizado al vincular se transcriben solo cuando el asistente los abre.
- **Videos**: se transcribe su audio (hasta 25 MB). No se analizan las imágenes del video.
- **Documentos**: se leen los de texto plano, imagen y audio. PDF y Office todavía no.

La transcripción envía el audio al servicio configurado (por defecto, OpenAI). Si no quieres eso, deja `WHISPER_API_KEY` vacía o apunta `WHISPER_BASE_URL` a un servicio propio compatible.

## Notas de voz

Con `ELEVENLABS_API_KEY` y `ELEVENLABS_VOICE_ID` definidas aparece la tool `send_voice_note`: el asistente escribe el texto, ElevenLabs lo convierte en audio con esa voz y se entrega como nota de voz (Ogg/Opus). El texto queda en el historial como transcripción del mensaje enviado.

- El voice ID se copia en ElevenLabs desde *Voices* → menú de la voz → *Copy voice ID*.
- Cada nota admite hasta 5000 caracteres y consume créditos de la cuenta de ElevenLabs. El texto se envía a ese servicio.
- Sin las dos variables la tool no se ofrece.

## Configuración

| Variable | Por defecto | |
|---|---|---|
| `DATA_DIR` | `./data` (`/data` en Docker) | Bases de datos SQLite y socket de administración |
| `LISTEN_ADDR` | `127.0.0.1:4020` | Dirección de escucha |
| `PUBLIC_URL` | de las cabeceras del proxy | Origen público https |
| `RETENTION_DAYS` | `0` | Borra mensajes más antiguos (0 = nunca) |
| `WHISPER_API_KEY` | vacío | Activa la transcripción (`OPENAI_API_KEY` también vale) |
| `WHISPER_BASE_URL` | `https://api.openai.com/v1` | API compatible con OpenAI |
| `WHISPER_MODEL` | `whisper-1` | Modelo de transcripción |
| `WHISPER_LANGUAGE` | autodetectar | Pista de idioma, p. ej. `es` |
| `WHISPER_AUTO` | `true` | Transcribir al llegar |
| `WHISPER_MAX_SECONDS` | `600` | Duración máxima para la transcripción automática |
| `ELEVENLABS_API_KEY` | vacío | Con `ELEVENLABS_VOICE_ID`, activa las notas de voz |
| `ELEVENLABS_VOICE_ID` | vacío | Voz de ElevenLabs con la que se leen |
| `ELEVENLABS_MODEL` | `eleven_multilingual_v2` | Modelo de texto a voz |
| `STORE_STATUS` | `false` | Guarda los estados de tus contactos como un chat aparte |

## Qué se guarda y qué no

En `DATA_DIR` quedan tres archivos SQLite: `whatsapp.db` (la sesión de WhatsApp), `messages.db` (cuentas y mensajes) y `oauth.db` (clientes OAuth y hashes de tokens).

- Se guarda el texto de mensajes recibidos y enviados, el historial que WhatsApp sincroniza al vincular, y las ediciones y eliminaciones "para todos".
- **Reacciones**: se guarda la reacción vigente de cada persona en cada mensaje.
- **Votos de encuestas**: se guarda el voto vigente de cada persona. Los votos llegan cifrados con una clave que viaja con la encuesta, así que los de encuestas anteriores a la vinculación pueden no poder leerse.
- **Estados**: no se guardan salvo con `STORE_STATUS=true`. En ese caso aparecen en el chat `status@broadcast` ("Status updates") y entran en la búsqueda. Sus fotos y videos solo se pueden abrir mientras WhatsApp los conserva (unas 24 horas).
- **Canales**: no se capturan al llegar. `get_channel_messages` los consulta a WhatsApp cuando el asistente los pide, y lo que se haya consultado queda guardado para poder abrir sus archivos y buscarlo.
- Las tools de envío solo escriben a personas y grupos: no publican estados ni en canales.
- Los tokens de cuenta y de OAuth se guardan solo como hash.

`messages.db` contiene conversaciones privadas en texto plano y `whatsapp.db` permite actuar como tu cuenta: protege ese directorio y sus copias de seguridad. Las tools de envío entregan mensajes reales.

## Desarrollo

```bash
go test ./...
go run ./cmd/wa-mcp serve
```

Este proyecto usa una biblioteca no oficial de WhatsApp. El uso automatizado puede ir contra los términos de WhatsApp y, en casos de abuso, llevar al bloqueo del número.
