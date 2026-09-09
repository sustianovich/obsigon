---
titulo: Sistemas de información
estado: vigente
tags:
  - hospital
  - soporte
  - sistemas-de-informacion
  - informatica
descripcion: Aplicaciones del hospital (HIS, HCE, LIS, RIS, PACS), integración, explotación de datos y seguridad.
tipo: sistema
version: "1.0"
autor: Documentación de ejemplo
fecha_creacion: 2026-08-10
derechos: ejemplo-libre
---

# Sistemas de información

El servicio de sistemas de información o informática sanitaria mantiene las aplicaciones y la
infraestructura con las que trabaja el hospital, y convierte la actividad registrada en datos
explotables para dirección, calidad y auditoría.

## Objetivo

Describir el mapa de aplicaciones del hospital, cómo se integran entre sí y qué exige la
protección de los datos de salud.

## Alcance

Aplica a los sistemas asistenciales y de gestión del centro. Queda fuera la infraestructura
de red y comunicaciones corporativa cuando depende de un servicio externo.

## Desarrollo

### Mapa de aplicaciones

| Sigla | Sistema | Cubre |
|---|---|---|
| HIS | Sistema de información hospitalaria | Núcleo: pacientes, episodios, citas, camas |
| HCE | Historia clínica electrónica | Evolutivos, órdenes, informes |
| LIS | Sistema del laboratorio | Peticiones y resultados de [[Laboratorio clínico|laboratorio]] |
| RIS / PACS | Radiología e imagen | Citas, informes y almacén de imágenes |
| SAP-AP | Anatomía patológica | Muestras, bloques e informes |
| ERP | Gestión económica | Compras, almacén, personal |

### Integración

Ningún sistema vive aislado: se comunican mediante mensajería estándar (HL7, FHIR, y DICOM
para imagen) a través de un motor de integración. Cuando una petición de analítica no llega,
el problema suele estar aquí y no en el servicio de destino.

### Explotación de datos

Extracción y consulta sobre el data warehouse del hospital, cuadros de mando de indicadores,
informes para la memoria anual y respuesta a peticiones de auditoría. La codificación que
produce [[Admisión y documentación clínica|Admisión y documentación clínica]] es la materia
prima de casi todos estos indicadores.

### Seguridad y protección de datos

Control de accesos por perfil, trazabilidad de quién consulta qué historia, copias de
seguridad, continuidad ante caída de sistemas y cumplimiento de la normativa de protección de
datos de salud, que son datos de categoría especial.

### Indicadores habituales

Disponibilidad de los sistemas críticos, incidencias abiertas y tiempo de resolución, mensajes
de integración fallidos y accesos indebidos detectados.

## Documentos relacionados

- [[Admisión y documentación clínica]]
- [[Laboratorio clínico]]
- [[Diagnóstico por imagen]]

## Observaciones

> [!warning] Circuito degradado
> Toda sección necesita un procedimiento en papel para seguir trabajando cuando el sistema no
> está disponible. Es tan parte del servicio como la aplicación misma.

## Control de cambios

| Versión | Fecha | Descripción del cambio | Responsable |
|---|---|---|---|
| 1.0 | 2026-08-10 | Creación inicial | Documentación de ejemplo |